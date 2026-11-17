const fs = require('fs');
const path = require('path');
const yaml = require('js-yaml');

/**
 * Auto-assigns maintainers to incoming Pull Requests: the approvers of the
 * areas (governance/areas.yaml) covering the changed files, filtered to
 * active maintainers (governance/maintainers.yaml), with a randomized roster
 * fallback.
 *
 * This function is executed as a GitHub Action. It parses the active
 * maintainers roster, filters out the PR author and bots, and assigns up to
 * two reviewers: prioritizing approvers from the areas covering the changed
 * files (both seats when the area has them), and only then randomly
 * selecting fallback reviewer(s) from the remaining eligible roster
 * to encourage knowledge sharing.
 *
 * @param {{
 *   github: !Object,
 *   context: !Object,
 *   core: !Object,
 *   delayMs: (number|undefined),
 * }} params - The injected actions/github-script objects.
 *     github: An authenticated Octokit REST client.
 *     context: The GitHub Actions workflow context and payload.
 *     core: The GitHub Actions core toolkit for logging/errors.
 *     delayMs: How long to wait before assigning, so humans can pick
 *         reviewers first. Defaults to 5 minutes.
 */
module.exports = async ({github, context, core, delayMs = 5 * 60 * 1000}) => {
  const {owner, repo} = context.repo;
  const prNum = context.payload.pull_request.number;

  // Filter out automated PRs
  if (context.payload.pull_request.user.type === 'Bot') {
    return;
  }

  // Give humans time to pick reviewers manually before the bot steps in
  if (delayMs > 0) {
    console.log(`Waiting ${delayMs / 1000}s before assigning PR #${prNum}`);
    await new Promise(resolve => setTimeout(resolve, delayMs));
  }

  // Fetch the latest PR state
  const {data: pr} =
      await github.rest.pulls.get({owner, repo, pull_number: prNum});
  const author = pr.user.login;
  const authorLower = author.toLowerCase();

  if (pr.state !== 'open' || pr.draft) {
    return;
  }

  // Check if already assigned
  if ((pr.requested_reviewers || []).length > 0 ||
      (pr.requested_teams || []).length > 0 ||
      (pr.assignees || []).length > 0) {
    console.log(`PR #${prNum} already has reviewers or assignees; skipping.`);
    return;
  }

  // Read active maintainers and areas from governance YAML files
  let maintainersData, areasData;
  try {
    const workspace = process.env.GITHUB_WORKSPACE || '.';
    maintainersData = yaml.load(
        fs.readFileSync(
            path.join(workspace, 'governance/maintainers.yaml'), 'utf8'));
    areasData = yaml.load(
        fs.readFileSync(
            path.join(workspace, 'governance/areas.yaml'), 'utf8'));
  } catch (error) {
    core.setFailed(`Could not load governance YAML files: ${error.message}`);
    return;
  }

  // Active maintainers (status from maintainers.yaml) form the general pool;
  // each area's approvers (from areas.yaml) form its specialist pool. Only
  // active maintainers are ever assigned, whichever list names them.
  const activeLogins = new Set();
  const eligibleApprovers = [];
  for (const m of maintainersData?.maintainers || []) {
    if (m.status === 'ACTIVE' && m.github && m.github.toLowerCase() !== authorLower) {
      activeLogins.add(m.github.toLowerCase());
      eligibleApprovers.push(m.github);
    }
  }
  const maintainersByArea = new Map();
  for (const area of areasData?.areas || []) {
    maintainersByArea.set(
        area.name,
        (area.approvers || []).filter(login => activeLogins.has(login.toLowerCase())));
  }

  if (eligibleApprovers.length === 0) {
    core.setFailed('No eligible approvers available to assign.');
    return;
  }

  // Find all specialists for areas covering the changed files
  let eligibleAreaSpecialists = [];
  try {
    const changedFiles = await github.paginate(github.rest.pulls.listFiles, {
      owner,
      repo,
      pull_number: prNum,
      per_page: 100,
    });

    const filePaths = changedFiles.map(f => '/' + f.filename);
    const matchedAreas = (areasData?.areas || [])
        .filter(area => area.paths?.some(p => filePaths.some(f => f === p || f.startsWith(p + '/'))))
        .map(area => area.name);

    eligibleAreaSpecialists = [
      ...new Set(matchedAreas.flatMap(area => maintainersByArea.get(area) || [])),
    ];
  } catch (error) {
    core.warning(
        `Notice: Could not match changed files to areas: ${error.message}`);
  }

  const shuffle = (array) => {
    const arr = [...array];
    for (let i = arr.length - 1; i > 0; i--) {
      const j = Math.floor(Math.random() * (i + 1));
      [arr[i], arr[j]] = [arr[j], arr[i]];
    }
    return arr;
  };

  const selectedReviewers = [];

  // Prioritize 1 specialist from the matched areas
  if (eligibleAreaSpecialists.length > 0) {
    const specialistsToAssign = shuffle(eligibleAreaSpecialists).slice(0, 1);
    for (const specialist of specialistsToAssign) {
      selectedReviewers.push(specialist);
      console.log(`Selected area specialist: ${specialist}`);
    }
  }

  // Fill remaining slot(s) (up to 2 total reviewers), preferring the matched
  // areas: a second specialist from the same areas if one is available, and
  // only then the general eligible roster. This keeps both seats inside the
  // area whenever the area has the people for it.
  const selectedReviewersLower = selectedReviewers.map(r => r.toLowerCase());
  const notSelected = login => !selectedReviewersLower.includes(login.toLowerCase());
  const remainingSpecialists = eligibleAreaSpecialists.filter(notSelected);
  const remainingGeneral = eligibleApprovers.filter(
      login => notSelected(login) &&
          !remainingSpecialists.some(s => s.toLowerCase() === login.toLowerCase()));
  const remainingEligible = [
    ...shuffle(remainingSpecialists),
    ...shuffle(remainingGeneral),
  ];
  if (remainingEligible.length > 0 && selectedReviewers.length < 2) {
    const needed = 2 - selectedReviewers.length;
    const fallbackToAssign = remainingEligible.slice(0, needed);
    for (const reviewer of fallbackToAssign) {
      selectedReviewers.push(reviewer);
      const kind = remainingSpecialists.includes(reviewer) ?
          'area specialist' : 'fallback roster reviewer';
      console.log(`Selected ${kind}: ${reviewer}`);
    }
  }

  // Apply assignment
  try {
    await github.rest.pulls.requestReviewers(
        {owner, repo, pull_number: prNum, reviewers: selectedReviewers});
    console.log(`Successfully requested review from [${
        selectedReviewers.join(', ')}] for PR #${prNum}`);
  } catch (error) {
    core.setFailed(`Failed to apply assignment API calls: ${error.message}`);
    return;
  }
};
