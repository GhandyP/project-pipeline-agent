"use strict";

// The UI never builds markup from server data: every value goes through
// textContent, so a project name or a model summary cannot become HTML.

const state = {
  projects: [],
  selected: null,
  snapshot: null,
  findings: null,
  review: null,
  streamFailed: false,
};

const CHANGE_FIELDS = ["owner_id", "status", "due_date", "progress_percent", "blocked_reason"];
const APPROVER_KEY = "project-pipeline-agent.approver";

const $ = (id) => document.getElementById(id);

function el(tag, props = {}, children = []) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (key === "class") node.className = value;
    else if (key === "text") node.textContent = value;
    else if (key === "onclick") node.addEventListener("click", value);
    else if (value !== undefined && value !== null) node.setAttribute(key, value);
  }
  for (const child of children) {
    node.append(child);
  }
  return node;
}

function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

function setStatus(message, kind) {
  const banner = $("status");
  banner.textContent = message || "";
  banner.className = kind ? `banner ${kind}` : "banner";
  banner.hidden = !message;
}

async function api(path, options) {
  const response = await fetch(path, options);
  const body = await response.text();
  let payload = null;
  if (body) {
    try {
      payload = JSON.parse(body);
    } catch {
      payload = null;
    }
  }
  if (!response.ok) {
    const error = new Error((payload && payload.error) || `request failed with ${response.status}`);
    error.status = response.status;
    error.payload = payload;
    throw error;
  }
  return payload;
}

// --------------------------------------------------------------------------
// rendering
// --------------------------------------------------------------------------

function renderProjects() {
  const list = $("project-list");
  clear(list);
  for (const entry of state.projects) {
    const summary = entry.summary;
    const button = el("button", {
      type: "button",
      "aria-current": String(state.selected === summary.project_id),
      onclick: () => selectProject(summary.project_id),
    }, [
      el("span", { text: `${summary.name} (${summary.project_id}) v${summary.version}` }),
      el("span", { class: `badge ${entry.health}`, text: `${entry.health} · ${entry.blockers} blocker(s)` }),
    ]);
    list.append(el("li", {}, [button]));
  }
  if (!state.projects.length) {
    list.append(el("li", { class: "muted", text: "No projects in the store yet." }));
  }
}

function renderSnapshot() {
  const snapshot = state.snapshot;
  const meta = $("snapshot-meta");
  const container = $("milestones");
  clear(container);
  if (!snapshot) {
    meta.textContent = "Select a project.";
    return;
  }
  meta.textContent = `${snapshot.name} · version ${snapshot.version} · updated ${new Date(snapshot.updated_at).toISOString().slice(0, 10)} · team: ${(snapshot.team || []).map((owner) => owner.name || owner.id).join(", ") || "none"}`;

  const head = el("tr", {}, [
    el("th", { text: "Milestone" }),
    el("th", { text: "Status" }),
    el("th", { text: "Owner" }),
    el("th", { text: "Due" }),
    el("th", { text: "Progress" }),
    el("th", { text: "Depends on" }),
  ]);
  const rows = (snapshot.milestones || []).map((milestone) => el("tr", {}, [
    el("td", { text: `${milestone.name} (${milestone.id})` }),
    el("td", { text: milestone.status }),
    el("td", { text: milestone.owner && milestone.owner.id ? milestone.owner.name || milestone.owner.id : "unowned" }),
    el("td", { text: milestone.due_date ? milestone.due_date.slice(0, 10) : "—" }),
    el("td", { text: `${milestone.progress_percent}%` }),
    el("td", { text: (milestone.depends_on || []).join(", ") || "—" }),
  ]));
  container.append(el("table", {}, [el("thead", {}, [head]), el("tbody", {}, rows)]));
}

function blockerCard(blocker) {
  const evidence = (blocker.evidence || []).map((item) =>
    el("p", { class: "evidence", text: `${item.rule} · ${item.milestone_id || "-"} · ${item.detail}` }));
  return el("li", { class: `card ${blocker.severity}` }, [
    el("p", {}, [
      el("strong", { text: `${blocker.severity.toUpperCase()} · ${blocker.kind}` }),
      el("span", { text: ` — ${blocker.summary}` }),
    ]),
    el("p", { class: "muted", text: `blocker id ${blocker.id} · owner state ${blocker.owner_status}${blocker.days_overdue ? ` · ${blocker.days_overdue} day(s) late` : ""}` }),
    ...evidence,
  ]);
}

function renderFindings() {
  const list = $("findings");
  clear(list);
  const findings = state.findings;
  if (!findings) return;
  if (!findings.blockers || !findings.blockers.length) {
    list.append(el("li", { class: "muted", text: "No blockers derived from this snapshot." }));
    return;
  }
  for (const blocker of findings.blockers) {
    list.append(blockerCard(blocker));
  }
}

function renderReview(review) {
  const out = $("review-out");
  clear(out);
  if (!review) return;
  out.append(el("p", { class: "review-summary" }, [el("strong", { text: "Summary: " }), el("span", { text: review.summary })]));

  const actions = review.next_actions || [];
  if (actions.length) {
    out.append(el("h3", { text: "Ranked next actions" }));
    const items = actions.map((action) => el("li", {}, [
      el("span", { text: action.action }),
      el("span", { class: "muted", text: ` — cites ${(action.blocker_ids || []).join(", ")}${action.owner && action.owner.id ? ` · owner ${action.owner.name || action.owner.id}` : ""}` }),
    ]));
    out.append(el("ol", { class: "actions" }, items));
  }

  const rejections = review.rejections || [];
  if (rejections.length) {
    out.append(el("h3", { text: "Refused by the deterministic gate" }));
    out.append(el("ul", { class: "findings" }, rejections.map((rejection) =>
      el("li", { class: "card low" }, [
        el("p", { text: `${rejection.kind}: ${rejection.subject}` }),
        el("p", { class: "evidence", text: rejection.reason }),
      ]))));
  }
  out.append(el("p", { class: "muted", text: `Findings come from version ${review.findings.version} of the project. No project data was written; any proposal above is recorded in the audit trail.` }));
}

function changeTable(proposal) {
  const head = el("tr", {}, [el("th", { text: "Milestone" }), el("th", { text: "Field" }), el("th", { text: "Before" }), el("th", { text: "After" })]);
  const rows = (proposal.changes || []).map((change) => el("tr", {}, [
    el("td", { text: change.milestone_id }),
    el("td", { text: change.field }),
    el("td", { class: "before", text: change.before || "—" }),
    el("td", { class: "after", text: change.after || "—" }),
  ]));
  return el("table", { class: "diff" }, [el("thead", {}, [head]), el("tbody", {}, rows)]);
}

function proposalCard(proposal, source) {
  const approverInput = el("input", {
    type: "text",
    placeholder: "approver name",
    value: approverName(),
    "aria-label": "Approver name",
  });
  approverInput.addEventListener("change", () => saveApproverName(approverInput.value));

  const card = el("li", { class: "card low" }, [
    el("p", {}, [
      el("strong", { text: `Dry run ${proposal.update_id}` }),
      el("span", { class: "muted", text: ` · expected version ${proposal.expected_version} · ${source}` }),
    ]),
    el("p", { class: "muted", text: proposal.rationale || "no rationale given" }),
    changeTable(proposal),
    el("div", { class: "decision" }, [
      approverInput,
      el("button", { type: "button", onclick: () => decide(proposal, true, approverInput) }, [document.createTextNode("Approve and apply")]),
      el("button", { class: "secondary", type: "button", onclick: () => decide(proposal, false, approverInput) }, [document.createTextNode("Decline")]),
    ]),
  ]);
  return card;
}

function renderProposals(proposals, source = "proposed by the agent") {
  const container = $("proposals");
  clear(container);
  if (!proposals || !proposals.length) {
    container.append(el("p", { class: "muted", text: "No dry-run update is waiting for a decision." }));
    return;
  }
  const list = el("ul", { class: "findings" });
  for (const proposal of proposals) {
    list.append(proposalCard(proposal, source));
  }
  container.append(list);
}

function renderAudit(records) {
  const list = $("audit");
  clear(list);
  if (!records || !records.length) {
    list.append(el("li", { class: "muted", text: "Nothing decided yet." }));
    return;
  }
  for (const record of records) {
    const when = new Date(record.at).toISOString().replace("T", " ").slice(0, 19);
    const who = record.approval && record.approval.by ? record.approval.by : "—";
    list.append(el("li", {}, [
      el("strong", { text: `${record.action} ` }),
      el("span", { text: `${record.update_id} · by ${who} · ${when}` }),
      el("span", { class: "muted", text: record.resulting_version ? ` · version ${record.expected_version} → ${record.resulting_version}` : ` · version ${record.expected_version}` }),
    ]));
  }
}

function renderManualForm() {
  const milestoneSelect = $("manual-milestone");
  const fieldSelect = $("manual-field");
  clear(milestoneSelect);
  clear(fieldSelect);
  for (const milestone of (state.snapshot && state.snapshot.milestones) || []) {
    milestoneSelect.append(el("option", { value: milestone.id, text: `${milestone.id} — ${milestone.name}` }));
  }
  for (const field of CHANGE_FIELDS) {
    fieldSelect.append(el("option", { value: field, text: field }));
  }
}

// --------------------------------------------------------------------------
// actions
// --------------------------------------------------------------------------

async function loadProjects() {
  const payload = await api("/api/projects");
  state.projects = payload.projects || [];
  renderProjects();
}

async function selectProject(projectID) {
  try {
    const payload = await api(`/api/projects/${projectID}`);
    state.selected = projectID;
    state.snapshot = payload.snapshot;
    state.findings = payload.findings;
    state.review = null;
    renderProjects();
    renderSnapshot();
    renderFindings();
    renderReview(null);
    renderProposals([]);
    renderManualForm();
    $("run-review").disabled = false;
    setStatus(`Reviewing ${projectID} at version ${payload.snapshot.version}.`);
    await loadAudit();
  } catch (error) {
    setStatus(error.message, "error");
  }
}

async function loadAudit() {
  if (!state.selected) return;
  const payload = await api(`/api/projects/${state.selected}/audit`);
  renderAudit(payload.records || []);
}

function appendStream(kind, payload) {
  const list = $("stream");
  const detail = payload.args || payload.result || "";
  const node = el("li", {}, [el("span", { class: "kind", text: kind })]);
  if (payload.text) {
    node.append(el("span", { text: payload.text }));
  } else if (payload.tool) {
    node.append(el("span", { class: "tool", text: payload.tool }));
    if (detail) {
      node.append(el("details", {}, [
        el("summary", { text: "payload" }),
        el("pre", { text: detail.slice(0, 2000) }),
      ]));
    }
  } else if (payload.error) {
    node.append(el("span", { class: "before", text: payload.error }));
  }
  list.append(node);
}

function handleSSE(raw) {
  let event = "message";
  const dataLines = [];
  for (const line of raw.split("\n")) {
    if (line.startsWith("event:")) event = line.slice(6).trim();
    else if (line.startsWith("data:")) dataLines.push(line.slice(5).trim());
  }
  if (!dataLines.length) return;
  let payload;
  try {
    payload = JSON.parse(dataLines.join("\n"));
  } catch {
    return;
  }
  appendStream(event, payload);
  if (event === "review" && payload.review) {
    state.review = payload.review;
    renderReview(payload.review);
    renderProposals(payload.review.proposals || [], "proposed by the agent");
  }
  if (event === "error") {
    state.streamFailed = true;
    setStatus(payload.error || "the review failed", "error");
  }
}

async function runReview() {
  if (!state.selected) return;
  clear($("stream"));
  state.streamFailed = false;
  $("run-review").disabled = true;
  setStatus(`Running the agent review of ${state.selected}.`);
  try {
    const response = await fetch(`/api/projects/${state.selected}/review`, { method: "POST" });
    if (!response.ok || !response.body) {
      const payload = await response.json().catch(() => null);
      throw new Error((payload && payload.error) || `the review failed with ${response.status}`);
    }
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let boundary = buffer.indexOf("\n\n");
      while (boundary >= 0) {
        const raw = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + 2);
        handleSSE(raw);
        boundary = buffer.indexOf("\n\n");
      }
    }
    if (state.streamFailed) {
      // The stream carried an error event: the run did not produce a review,
      // so the last word stays the error.
      setStatus("The review stopped with an error. No project data was written.", "error");
    } else {
      setStatus("Review finished. No project data was written.", "ok");
    }
  } catch (error) {
    setStatus(error.message, "error");
  } finally {
    $("run-review").disabled = false;
  }
}

async function decide(proposal, approve, approverInput) {
  const by = approverInput.value.trim();
  if (by.length < 2) {
    setStatus("Type the approver name (at least two characters) before deciding.", "error");
    approverInput.focus();
    return;
  }
  saveApproverName(by);
  try {
    const payload = await api(`/api/projects/${state.selected}/updates/${proposal.update_id}/decision`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ approve, by, note: "" }),
    });
    if (approve && payload.applied) {
      setStatus(`Applied ${payload.applied.update_id} by ${payload.applied.approval.by}: version ${payload.applied.expected_version} → ${payload.applied.resulting_version}.`, "ok");
    } else {
      setStatus(`Declined ${proposal.update_id} by ${payload.record.approval.by}. The project was not touched.`);
    }
  } catch (error) {
    if (error.status === 409) {
      setStatus(`Refused: ${error.message}. The project changed, so nothing was written. Run the review again.`, "error");
    } else {
      setStatus(error.message, "error");
    }
  }
  await refreshSelected();
}

async function refreshSelected() {
  if (!state.selected) return;
  const payload = await api(`/api/projects/${state.selected}`);
  state.snapshot = payload.snapshot;
  state.findings = payload.findings;
  renderProjects();
  renderSnapshot();
  renderFindings();
  renderManualForm();
  await loadProjects();
  await loadAudit();
}

async function submitManualProposal(event) {
  event.preventDefault();
  if (!state.selected) return;
  const changes = [{
    milestone_id: $("manual-milestone").value,
    field: $("manual-field").value,
    value: $("manual-value").value,
  }];
  try {
    const payload = await api(`/api/projects/${state.selected}/updates`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ rationale: $("manual-rationale").value, changes }),
    });
    renderProposals([payload.proposal], "proposed by you");
    setStatus(`Dry run ${payload.proposal.update_id} prepared against version ${payload.proposal.expected_version}.`);
    await loadAudit();
  } catch (error) {
    const rejections = (error.payload && error.payload.rejections) || [];
    const reasons = rejections.map((item) => `${item.subject}: ${item.reason}`).join(" · ");
    setStatus(reasons ? `${error.message} — ${reasons}` : error.message, "error");
  }
}

function approverName() {
  try {
    return localStorage.getItem(APPROVER_KEY) || "";
  } catch {
    return "";
  }
}

function saveApproverName(value) {
  try {
    localStorage.setItem(APPROVER_KEY, value);
  } catch {
    // A browser without storage still works; the name is typed per decision.
  }
}

async function main() {
  $("run-review").addEventListener("click", runReview);
  $("manual-propose").addEventListener("submit", submitManualProposal);
  $("approver").value = approverName();
  $("approver").addEventListener("change", (event) => saveApproverName(event.target.value));
  try {
    await loadProjects();
    const first = state.projects[0];
    if (first) {
      await selectProject(first.summary.project_id);
    } else {
      setStatus("No projects in the store. Seed it with `pipeline serve --seed testdata/projects` or the demo command.");
    }
  } catch (error) {
    setStatus(error.message, "error");
  }
}

document.addEventListener("DOMContentLoaded", main);
