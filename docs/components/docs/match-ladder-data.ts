/** How tug ranks matches for the query "api", best first. */
export const rungs = [
  { rank: 'Exact', example: 'api', means: 'The name is exactly what you typed.' },
  { rank: 'Exact, any case', example: 'API', means: 'Same letters, different capitals.' },
  { rank: 'Prefix', example: 'api-server', means: 'The name starts with what you typed.' },
  { rank: 'Word prefix', example: 'payments-api', means: 'A word inside the name starts with it.' },
  { rank: 'Substring', example: 'rapid', means: 'It appears anywhere in the name.' },
  { rank: 'Fuzzy', example: 'a-p-i-gateway', means: 'Its letters appear in order, with gaps.' },
];
