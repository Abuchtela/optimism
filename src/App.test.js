import React from 'react';
import { render, screen } from '@testing-library/react';
import App from './App';

test('renders header component', () => {
  render(<App />);
  const headerElement = screen.getByText(/Application Title/i);
  expect(headerElement).toBeInTheDocument();
});

test('renders main component', () => {
  render(<App />);
  const mainElement = screen.getByText(/Main Content/i);
  expect(mainElement).toBeInTheDocument();
});

test('renders footer component', () => {
  render(<App />);
  const footerElement = screen.getByText(/© 2023 Your Company/i);
  expect(footerElement).toBeInTheDocument();
});
