// Posts or updates a single PR comment summarising CI job results.
// Invoked by ci.yml via actions/github-script script-path (keeps JS out of YAML).
//
// Supports both pull_request and push events. On push events (which fire when
// the base branch workflow still has a branches filter that prevents the
// pull_request event from triggering), the PR number is resolved from the
// branch name via the GitHub API. If no open PR exists for the branch, the
// script exits silently — no comment is posted.
const needs = JSON.parse(process.env.NEEDS_JSON || '{}');

function icon(r) {
  if (r === 'success') return '✅';
  if (r === 'skipped') return '⏭️';
  if (r === 'cancelled') return '🚫';
  return '❌';
}

const coveragePct = needs['validate-test']?.outputs?.pct;

const rows = [
  ['Tests & race detector', 'validate-test'],
  ['Image build + Dockerfile lint', 'build-image'],
  ['Coverage ≥ 95%',         'validate-test'],
  ['Trivy CVE scan',         'trivy'],
  ['Smoke tests',            'smoke'],
  ['Code quality + Helm + CD-19', 'validate-quality'],
];

const table = rows
  .map(function (row) {
    const label = row[0];
    const key   = row[1];
    const r     = needs[key]?.result || 'skipped';
    const value = (label === 'Coverage ≥ 95%' && coveragePct)
      ? parseFloat(coveragePct).toFixed(1) + '%'
      : '`' + r + '`';
    return '| ' + icon(r) + ' | ' + label + ' | ' + value + ' |';
  })
  .join('\n');

const allPassed = rows.every(function (row) {
  return needs[row[1]]?.result === 'success';
});
const headline = allPassed
  ? '✅ All checks passed — ready to merge'
  : '❌ Some checks failed';

// Resolve the PR number. On pull_request events context.issue.number is set
// directly. On push events (feature branch push with an open PR) we look up
// the PR by head branch name. Exit silently if no open PR is found.
let prNumber = context.issue?.number;
if (!prNumber) {
  const branch = context.ref?.replace('refs/heads/', '') || context.payload?.ref?.replace('refs/heads/', '');
  if (branch) {
    const { data: prs } = await github.rest.pulls.list({
      ...context.repo,
      state: 'open',
      head: `${context.repo.owner}:${branch}`,
    });
    prNumber = prs[0]?.number;
  }
}
if (!prNumber) {
  core.info('No open PR found for this branch — skipping pr-summary comment.');
  return;
}

const marker = '<!-- ci-pr-summary -->';
const body = [
  marker,
  '## ' + headline,
  '',
  '| | Check | Result |',
  '|---|---|---|',
  table,
  '',
  '📦 Image cached for `linux/amd64` · merges to `main` are signed with Cosign',
  '🔍 [Security tab](https://github.com/' +
    context.repo.owner +
    '/' +
    context.repo.repo +
    '/security)',
].join('\n');

const { data: comments } = await github.rest.issues.listComments({
  ...context.repo,
  issue_number: prNumber,
});
const existing = comments.find(function (c) {
  return c.body?.startsWith(marker);
});

if (existing) {
  await github.rest.issues.updateComment({
    ...context.repo,
    comment_id: existing.id,
    body,
  });
} else {
  await github.rest.issues.createComment({
    ...context.repo,
    issue_number: prNumber,
    body,
  });
}
