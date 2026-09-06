"use strict";
const api = async (path, options = {}) => {
  const response = await fetch(path, {
    ...options,
    headers: { "Content-Type": "application/json", ...options.headers },
  });
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new Error(body.error || `Request failed (${response.status})`);
  }
  return response.status === 204 ? null : response.json();
};
const status = (form, text, error = false) => {
  const node = form.querySelector(".status");
  node.textContent = text;
  node.classList.toggle("error", error);
};
const waitlist = document.querySelector("#waitlist");
waitlist?.addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = waitlist.querySelector("button");
  button.disabled = true;
  try {
    await api("/v1/waitlist", {
      method: "POST",
      body: JSON.stringify({
        email: waitlist.email.value,
        company: waitlist.company.value,
        consent: true,
      }),
    });
    status(waitlist, "You're subscribed. Thanks for following Moirai.");
    waitlist.reset();
  } catch (error) {
    status(waitlist, error.message + ". Please try again.", true);
  } finally {
    button.disabled = false;
  }
});
// Matches the schema's canonical digest: recursively sorted keys, JSON numbers.
function canonical(value) {
  if (Array.isArray(value)) return "[" + value.map(canonical).join(",") + "]";
  if (value && typeof value === "object")
    return (
      "{" +
      Object.keys(value)
        .sort()
        .map((key) => JSON.stringify(key) + ":" + canonical(value[key]))
        .join(",") +
      "}"
    );
  return JSON.stringify(value);
}
const patterns = [
  /(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,}|AKIA[A-Z0-9]{16})/g,
];
function scrub(value) {
  if (typeof value === "string") {
    for (const pattern of patterns)
      value = value.replace(pattern, "[REDACTED]");
    return value;
  }
  if (Array.isArray(value)) return value.map(scrub);
  if (value && typeof value === "object")
    return Object.fromEntries(
      Object.entries(value).map(([key, child]) => [key, scrub(child)]),
    );
  return value;
}
let prepared, objectURL, requestKey;
const publish = document.querySelector("#publish");
document
  .querySelector("#archive")
  ?.addEventListener("change", async (event) => {
    prepared = undefined;
    requestKey = undefined;
    publish.querySelector("#reviewed").checked = false;
    const link = document.querySelector("#review-download");
    link.hidden = true;
    if (objectURL) URL.revokeObjectURL(objectURL);
    try {
      const file = event.target.files[0];
      if (!file) return;
      if (file.size > 32 * 1024 * 1024)
        throw new Error("Archive exceeds 32 MiB");
      const archive = JSON.parse(await file.text());
      if (
        archive.format !== "moirai.session" ||
        archive.version !== "1" ||
        archive.transcript?.schema_version !== "1.0"
      )
        throw new Error("Choose a version 1 .moirai archive");
      const originalDigest = [
        ...new Uint8Array(
          await crypto.subtle.digest(
            "SHA-256",
            new TextEncoder().encode(canonical(archive.transcript)),
          ),
        ),
      ]
        .map((b) => b.toString(16).padStart(2, "0"))
        .join("");
      if (archive.sha256 !== originalDigest)
        throw new Error("Archive integrity check failed");
      const transcript = scrub(archive.transcript);
      delete transcript.meta.cwd;
      if (transcript.meta.provenance)
        delete transcript.meta.provenance.source_cwd;
      for (const message of transcript.messages) {
        message.content = message.content
          .filter((block) => block.type !== "thinking")
          .map((block) => {
            if (block.source?.type === "path")
              return { type: "text", text: "[Local media omitted]" };
            if (block.artifact?.source?.type === "path")
              delete block.artifact.source;
            return block;
          });
        if (!message.content.length)
          message.content = [{ type: "text", text: "[Thinking omitted]" }];
      }
      const sha256 = [
        ...new Uint8Array(
          await crypto.subtle.digest(
            "SHA-256",
            new TextEncoder().encode(canonical(transcript)),
          ),
        ),
      ]
        .map((b) => b.toString(16).padStart(2, "0"))
        .join("");
      prepared = {
        format: "moirai.session",
        version: "1",
        created_at: new Date().toISOString(),
        transcript,
        sha256,
      };
      objectURL = URL.createObjectURL(
        new Blob([JSON.stringify(prepared, null, 2)], {
          type: "application/json",
        }),
      );
      link.href = objectURL;
      link.download = "reviewed.moirai";
      link.hidden = false;
      document.querySelector("#preview").textContent = JSON.stringify(
        transcript,
        null,
        2,
      ).slice(0, 100000);
      status(
        publish,
        "Prepared locally. Preview is limited to 100,000 characters; download the prepared archive for full review. No content has been uploaded.",
      );
    } catch (error) {
      prepared = undefined;
      status(publish, error.message, true);
    }
  });
publish?.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (!prepared)
    return status(publish, "Choose and review a valid archive first.", true);
  const button = publish.querySelector("button");
  button.disabled = true;
  const body = JSON.stringify({
    archive: prepared,
    visibility: document.querySelector("#visibility").value,
    team: document.querySelector("#team").value,
    expires: Number(document.querySelector("#expiry").value)
      ? Math.floor(Date.now() / 1000) +
        Number(document.querySelector("#expiry").value)
      : 0,
  });
  // Preserve the exact request on uncertain network outcomes, preventing duplicate publication.
  requestKey ||= {
    key: [...crypto.getRandomValues(new Uint8Array(24))]
      .map((b) => b.toString(16).padStart(2, "0"))
      .join(""),
    body,
  };
  try {
    const result = await api("/v1/publications", {
      method: "POST",
      headers: { "Idempotency-Key": requestKey.key },
      body: requestKey.body,
    });
    location.assign(result.url);
  } catch (error) {
    status(
      publish,
      error.message + ". Retrying submits the same checkpoint and settings.",
      true,
    );
    button.disabled = false;
  }
});
async function refresh() {
  const list = document.querySelector("#publications");
  if (!list) return;
  try {
    const entries = await api("/v1/publications");
    list.replaceChildren();
    if (!entries.length)
      list.textContent =
        "No publications yet. Your first checkpoint starts above.";
    for (const entry of entries) {
      const card = document.createElement("article");
      card.className = "message";
      const link = document.createElement("a");
      link.href = entry.url;
      link.textContent =
        entry.id.slice(0, 12) +
        " · " +
        entry.visibility +
        (entry.revoked ? " · revoked" : "");
      card.append(link);
      const actions = document.createElement("div");
      actions.className = "actions";
      for (const action of entry.can_manage
        ? ["revoke", "delete", "invite", "manage invitations"]
        : []) {
        const button = document.createElement("button");
        button.className = "secondary";
        button.textContent = action;
        button.addEventListener("click", async () => {
          try {
            if (action === "manage invitations") {
              const grants = await api(`/v1/publications/${entry.id}/grants`);
              if (!grants.length) {
                alert("No individual invitations.");
                return;
              }
              for (const grant of grants)
                if (
                  confirm(
                    `Remove @${grant.login}'s individual access grant? Team membership may still grant access.`,
                  )
                )
                  await api(`/v1/publications/${entry.id}/grants/${grant.id}`, {
                    method: "DELETE",
                  });
            } else if (action === "invite") {
              const login = prompt(
                "GitHub handle of an account that has already signed into Moirai:",
              );
              if (!login) return;
              await api(`/v1/publications/${entry.id}/grants`, {
                method: "POST",
                body: JSON.stringify({ login }),
              });
              alert("Access granted.");
            } else {
              if (
                !confirm(
                  `${action} this publication? Downloaded copies cannot be recalled.`,
                )
              )
                return;
              await api(
                `/v1/publications/${entry.id}${action === "revoke" ? "/revoke" : ""}`,
                { method: action === "delete" ? "DELETE" : "POST" },
              );
              await refresh();
            }
          } catch (error) {
            alert(error.message);
          }
        });
        actions.append(button);
      }
      card.append(actions);
      list.append(card);
    }
  } catch (error) {
    list.textContent = error.message;
  }
}
document.querySelector("#logout")?.addEventListener("click", async () => {
  try {
    await api("/v1/logout", { method: "POST" });
    location.assign("/");
  } catch (error) {
    alert(error.message);
  }
});
refresh();

async function refreshTeams() {
  const target = document.querySelector("#teams");
  if (!target) return;
  try {
    const teams = await api("/v1/teams");
    target.replaceChildren();
    const select = document.querySelector("#team");
    select.replaceChildren(new Option("Personal", ""));
    const identity = await api("/v1/me");
    const note = document.createElement("p");
    note.textContent = `Your account ID: ${identity.id}. Share this ID with a team owner to be invited.`;
    target.append(note);
    for (const team of teams) {
      if (team.role !== "reader") select.append(new Option(team.name, team.id));
      const card = document.createElement("article");
      card.className = "message";
      const title = document.createElement("h3");
      title.textContent = `${team.name} · ${team.role}`;
      card.append(title);
      const members = await api(`/v1/teams/${team.id}/members`);
      for (const member of members) {
        const row = document.createElement("p");
        row.textContent = `@${member.login} · ${member.role}`;
        if (team.role === "owner" && member.role !== "owner") {
          const remove = document.createElement("button");
          remove.textContent = "Remove member";
          remove.className = "secondary";
          remove.onclick = async () => {
            if (!confirm(`Remove @${member.login} from ${team.name}?`)) return;
            try {
              await api(`/v1/teams/${team.id}/members/${member.id}`, {
                method: "DELETE",
              });
              await refreshTeams();
            } catch (error) {
              alert(error.message);
            }
          };
          row.append(remove);
        }
        card.append(row);
      }
      if (team.role === "owner") {
        const invite = document.createElement("button");
        invite.textContent = "Add team member";
        invite.className = "secondary";
        invite.onclick = async () => {
          const user_id = prompt(
            "Account ID from your teammate's Moirai dashboard (or moirai whoami):",
          );
          if (!user_id) return;
          const role = prompt("Role: reader or writer", "reader");
          if (!role) return;
          try {
            await api(`/v1/teams/${team.id}/members`, {
              method: "POST",
              body: JSON.stringify({ user_id, role }),
            });
            await refreshTeams();
          } catch (error) {
            alert(error.message);
          }
        };
        card.append(invite);
      }
      target.append(card);
    }
  } catch (error) {
    target.textContent = error.message;
  }
}
document
  .querySelector("#create-team")
  ?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = event.target;
    try {
      await api("/v1/teams", {
        method: "POST",
        body: JSON.stringify({
          name: document.querySelector("#team-name").value,
        }),
      });
      form.reset();
      status(form, "Team created.");
      await refreshTeams();
    } catch (error) {
      status(form, error.message, true);
    }
  });
document.querySelector("#fork")?.addEventListener("click", async (event) => {
  const button = event.target;
  if (
    !confirm(
      "Create a private copy in your account with the original expiry? This copy remains independent of the original.",
    )
  )
    return;
  button.disabled = true;
  button.dataset.requestKey ||= [...crypto.getRandomValues(new Uint8Array(24))]
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
  try {
    const result = await api(
      `/v1/publications/${button.dataset.publication}/fork`,
      {
        method: "POST",
        headers: {
          "Idempotency-Key": button.dataset.requestKey,
        },
      },
    );
    location.assign(result.url);
  } catch (error) {
    document.querySelector("#fork-status").textContent =
      error.message === "authentication_required"
        ? "Sign in with GitHub before creating a fork."
        : error.message;
    button.disabled = false;
  }
});
refreshTeams();
