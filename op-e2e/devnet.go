package op_e2e

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/ethereum-optimism/optimism/op-chain-ops/genesis"
	"github.com/ethereum-optimism/optimism/op-e2e/e2eutils"
	"github.com/ethereum-optimism/optimism/op-node/client"
	"github.com/ethereum-optimism/optimism/op-node/eth"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-node/sources"
	"github.com/ethereum-optimism/optimism/op-node/testlog"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	gn "github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/require"
)

type Devnet struct {
	node          *gn.Node
	cancel        context.CancelFunc
	l2Engine      *sources.EngineClient
	L2Client      *ethclient.Client
	SystemConfig  eth.SystemConfig
	L1ChainConfig *params.ChainConfig
	L2ChainConfig *params.ChainConfig
	L1Head        *types.Block
	L2Head        *eth.ExecutionPayload
	sequenceNum   uint64
}

func NewDevnet(t *testing.T, cfg *genesis.DeployConfig) (*Devnet, error) {
	log := testlog.Logger(t, log.LvlCrit)
	l1Genesis, err := genesis.BuildL1DeveloperGenesis(cfg)
	require.Nil(t, err)
	l1Block := l1Genesis.ToBlock()

	l2Genesis, err := genesis.BuildL2DeveloperGenesis(cfg, l1Block)
	require.Nil(t, err)
	l2GenesisBlock := l2Genesis.ToBlock()

	rollupGenesis := rollup.Genesis{
		L1: eth.BlockID{
			Hash:   l1Block.Hash(),
			Number: l1Block.NumberU64(),
		},
		L2: eth.BlockID{
			Hash:   l2GenesisBlock.Hash(),
			Number: l2GenesisBlock.NumberU64(),
		},
		L2Time:       l2GenesisBlock.Time(),
		SystemConfig: e2eutils.SystemConfigFromDeployConfig(cfg),
	}

	node, _, err := initL2Geth("l2", big.NewInt(int64(cfg.L2ChainID)), l2Genesis, writeDefaultJWT(t))
	require.Nil(t, err)
	require.Nil(t, node.Start())

	auth := rpc.WithHTTPAuth(gn.NewJWTAuth(testingJWTSecret))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	l2Node, err := client.NewRPC(ctx, log, node.WSAuthEndpoint(), auth)
	require.Nil(t, err)

	// Finally create the engine client
	l2Engine, err := sources.NewEngineClient(
		l2Node,
		log,
		nil,
		sources.EngineClientDefaultConfig(&rollup.Config{Genesis: rollupGenesis}),
	)
	require.Nil(t, err)

	l2Client, err := ethclient.Dial(node.HTTPEndpoint())
	require.Nil(t, err)

	genesisPayload, err := eth.BlockAsPayload(l2GenesisBlock)

	require.Nil(t, err)
	return &Devnet{
		cancel:        cancel,
		node:          node,
		L2Client:      l2Client,
		l2Engine:      l2Engine,
		SystemConfig:  rollupGenesis.SystemConfig,
		L1ChainConfig: l1Genesis.Config,
		L2ChainConfig: l2Genesis.Config,
		L1Head:        l1Block,
		L2Head:        genesisPayload,
	}, nil
}

func (d *Devnet) Close() {
	d.node.Close()
	d.cancel()
	d.l2Engine.Close()
	d.L2Client.Close()
}

func (d *Devnet) AddL2Block(ctx context.Context, txs ...*types.Transaction) (*eth.ExecutionPayload, error) {
	attrs, err := d.CreatePayloadAttributes(txs)
	if err != nil {
		return nil, err
	}
	parentHash := d.L2Head.BlockHash
	fc := eth.ForkchoiceState{
		HeadBlockHash: parentHash,
		SafeBlockHash: parentHash,
	}
	res, err := d.l2Engine.ForkchoiceUpdate(ctx, &fc, attrs)
	if err != nil {
		return nil, err
	}
	if res.PayloadStatus.Status != eth.ExecutionValid {
		return nil, fmt.Errorf("forkChoiceUpdated gave unexpected status: %s", res.PayloadStatus.Status)
	}
	if res.PayloadID == nil {
		return nil, errors.New("forkChoiceUpdated returned nil PayloadID")
	}

	payload, err := d.l2Engine.GetPayload(ctx, *res.PayloadID)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(payload.Transactions, attrs.Transactions) {
		return nil, errors.New("required transactions were not included")
	}

	status, err := d.l2Engine.NewPayload(ctx, payload)
	if err != nil {
		return nil, err
	}
	if status.Status != eth.ExecutionValid {
		return nil, fmt.Errorf("newPayload returned unexpected status: %s", status.Status)
	}

	fc.HeadBlockHash = payload.BlockHash
	res, err = d.l2Engine.ForkchoiceUpdate(ctx, &fc, nil)
	if err != nil {
		return nil, err
	}
	if res.PayloadStatus.Status != eth.ExecutionValid {
		return nil, fmt.Errorf("forkChoiceUpdated gave unexpected status: %s", res.PayloadStatus.Status)
	}
	d.L2Head = payload
	d.sequenceNum = d.sequenceNum + 1
	return payload, nil
}

func (d *Devnet) CreatePayloadAttributes(txs []*types.Transaction) (*eth.PayloadAttributes, error) {
	l1Info, err := derive.L1InfoDepositBytes(d.sequenceNum, d.L1Head, d.SystemConfig)
	if err != nil {
		return nil, err
	}

	var txBytes []hexutil.Bytes
	txBytes = append(txBytes, l1Info)
	for _, tx := range txs {
		bin, err := tx.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("tx marshalling failed: %w", err)
		}
		txBytes = append(txBytes, bin)
	}
	attrs := eth.PayloadAttributes{
		Timestamp:    d.L2Head.Timestamp + 2,
		Transactions: txBytes,
		NoTxPool:     true,
		GasLimit:     (*eth.Uint64Quantity)(&d.SystemConfig.GasLimit),
	}
	return &attrs, nil
}