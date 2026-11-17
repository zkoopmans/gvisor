// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * @fileoverview Enforces the maintainer approval rule as a commit status.
 *
 * Rule (agreed 2026-11-16): a pull request authored by a maintainer needs an
 * approving review from one other maintainer; a pull request authored by
 * anyone else needs approving reviews from two maintainers. Nobody approves
 * their own change. "Maintainer" means a non-emeritus entry in
 * governance/maintainers.yaml on the base branch.
 *
 * Only approvals on the pull request's current head commit count, so a push
 * after approval resets the count (the ruleset's "require approval of the
 * most recent push" says the same thing; this makes the status agree with it).
 *
 * The result is published as the commit status `approvals/maintainer-rule`,
 * which the branch ruleset lists as a required status check. This workflow
 * never checks out or executes pull request content: it reads the roster from
 * the base branch and reviews from the API.
 *
 * @param {Object} options
 * @param {Object} options.github  Octokit client.
 * @param {Object} options.context Workflow context; the PR number comes from
 *     the triggering event or from `options.prNumber`.
 * @param {Object} options.core    Actions core helpers.
 * @param {number=} options.prNumber Explicit PR number (for workflow_run).
 */
module.exports = async ({github, context, core, prNumber}) => {
  const fs = require('fs');
  const path = require('path');
  const yaml = require('js-yaml');

  const {owner, repo} = context.repo;
  // The number is untrusted input on the workflow_run path (the recording
  // workflow runs in the pull request's context). It is only used as a lookup
  // key: everything the status is computed from comes from the API and the
  // base branch, so the worst a bad number can do is re-evaluate another PR.
  const pull_number = Number.isInteger(prNumber) && prNumber > 0 ?
      prNumber :
      (context.payload.pull_request && context.payload.pull_request.number);
  if (!pull_number) {
    core.setFailed('No pull request number in the event payload.');
    return;
  }

  const {data: pr} = await github.rest.pulls.get({owner, repo, pull_number});
  if (pr.state !== 'open') {
    console.log(`PR #${pull_number} is ${pr.state}; nothing to do.`);
    return;
  }
  const headSha = pr.head.sha;
  const author = (pr.user.login || '').toLowerCase();

  // Roster from the base branch of the checkout (never the PR head).
  const workspace = process.env.GITHUB_WORKSPACE || '.';
  const roster = yaml.load(fs.readFileSync(
      path.join(workspace, 'governance/maintainers.yaml'), 'utf8'));
  const maintainers = new Set();
  for (const m of roster.maintainers || []) {
    const status = String(m.status || '');
    if (m.github && !status.startsWith('EMERITUS_SINCE')) {
      maintainers.add(m.github.toLowerCase());
    }
  }

  const authorIsMaintainer = maintainers.has(author);
  const required = authorIsMaintainer ? 1 : 2;

  // Latest review per reviewer; only APPROVED/CHANGES_REQUESTED/DISMISSED
  // change the state (COMMENTED does not). Count approvals by maintainers
  // other than the author that are on the current head commit.
  const reviews = await github.paginate(github.rest.pulls.listReviews, {
    owner, repo, pull_number, per_page: 100,
  });
  const latest = new Map();
  for (const r of reviews) {
    if (!r.user || r.state === 'COMMENTED' || r.state === 'PENDING') continue;
    latest.set(r.user.login.toLowerCase(), r);
  }
  const approvers = [];
  for (const [login, r] of latest) {
    if (r.state !== 'APPROVED') continue;
    if (login === author) continue;
    if (!maintainers.has(login)) continue;
    if (r.commit_id !== headSha) continue;
    approvers.push(login);
  }
  approvers.sort();

  const ok = approvers.length >= required;
  const who = authorIsMaintainer ? 'a maintainer' : 'not a maintainer';
  const description = ok ?
      `${approvers.length}/${required} maintainer approval(s) on ${headSha.slice(0, 7)}` :
      `needs ${required} maintainer approval(s) (author is ${who}); have ${approvers.length} on ${headSha.slice(0, 7)}`;
  console.log(`PR #${pull_number} by ${author} (${who}): required=${required} approvers=[${approvers.join(', ')}] -> ${ok ? 'success' : 'pending'}`);

  await github.rest.repos.createCommitStatus({
    owner, repo, sha: headSha,
    state: ok ? 'success' : 'pending',
    context: 'approvals/maintainer-rule',
    description: description.slice(0, 140),
    target_url: `https://github.com/${owner}/${repo}/blob/${pr.base.ref}/GOVERNANCE.md`,
  });
};
