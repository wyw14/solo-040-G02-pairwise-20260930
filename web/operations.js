const { api, shortID, setMessage, withBusy } = window.LockFlow;
const form = document.querySelector("#plan-form");
const message = document.querySelector("#plan-message");
const waterwayForm = document.querySelector("#waterway-form");
const waterwayMessage = document.querySelector("#waterway-message");

const escapeHTML = (value) => String(value ?? "").replace(/[&<>"']/g, (ch) => ({
  "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
}[ch]));

async function refresh() {
  const [plans, chambers, health, waterways] = await Promise.all([
    api("/api/plans"), api("/api/chambers"), api("/healthz"), api("/api/waterway"),
  ]);
  document.querySelector("#health").textContent = health.status === "ok" ? "Devices online" : "Attention required";
  const rows = Object.entries(plans).flatMap(([chamber, items]) => items.map((plan) => `<tr><td>${chamber}</td><td>${shortID(plan.id)}</td><td>${plan.direction}</td><td>${plan.vessels.length}</td><td>${new Date(plan.created_at).toLocaleTimeString()}</td></tr>`));
  document.querySelector("#plans").innerHTML = rows.join("") || '<tr><td colspan="5" class="muted">No active plans</td></tr>';
  const current = chambers.find((item) => item.cycle);
  document.querySelector("#metric-chamber").textContent = current?.chamber_id || "Idle";
  document.querySelector("#metric-stage").textContent = current?.cycle?.stage || "-";
  document.querySelector("#metric-cycle").textContent = shortID(current?.cycle?.id);
  renderWaterway(waterways);
}

function renderWaterway(data) {
  const views = data.waterways || [];
  const occupants = views.flatMap((view) => (view.occupants || []).map((occ) => `
    <tr><td>${escapeHTML(occ.waterway_id)}</td><td>${escapeHTML(occ.convoy)}</td><td>${escapeHTML(occ.chamber_id)}</td>
    <td>${escapeHTML(occ.direction)}</td><td>${shortID(occ.cycle_id)}</td>
    <td>${new Date(occ.granted_at).toLocaleTimeString()}</td>
    <td><button class="danger" data-release="${escapeHTML(occ.chamber_id)}">Release</button></td></tr>`));
  document.querySelector("#waterway-occupants").innerHTML =
    occupants.join("") || '<tr><td colspan="7" class="muted">Shared water is clear</td></tr>';
  const waiters = views.flatMap((view) => (view.waitlist || []).map((denial) => `
    <tr><td>${escapeHTML(view.waterway_id)}</td><td>${escapeHTML(denial.requested_by)}</td>
    <td>${escapeHTML(denial.direction)}</td><td>${escapeHTML(denial.blocking_convoy)} (${escapeHTML(denial.blocking_chamber)})</td>
    <td>${escapeHTML(denial.blocking_heading)}</td><td>${escapeHTML(denial.reason)}</td></tr>`));
  document.querySelector("#waterway-waitlist").innerHTML =
    waiters.join("") || '<tr><td colspan="6" class="muted">No convoys blocked</td></tr>';
  document.querySelectorAll("[data-release]").forEach((button) => {
    button.onclick = async () => {
      await withBusy(button, async () => {
        try {
          await api(`/api/chambers/${button.dataset.release}/waterway/release`, { method: "POST", body: "{}" });
          setMessage(waterwayMessage, "");
          await refresh();
        } catch (error) { setMessage(waterwayMessage, error.message); }
      });
    };
  });
}

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = form.querySelector("button");
  await withBusy(button, async () => {
    try {
      const result = await api("/api/plans", { method: "POST", body: JSON.stringify({
        chamber_id: document.querySelector("#chamber").value,
        direction: document.querySelector("#direction").value,
        vessels: [{ name: document.querySelector("#vessel").value, call_sign: document.querySelector("#call-sign").value, length_m: Number(document.querySelector("#length").value) }],
      }) });
      setMessage(message, `Plan ${shortID(result.plan.id)} admitted to ${result.plan.chamber_id}.`);
      await refresh();
    } catch (error) { setMessage(message, error.message); }
  });
});

waterwayForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = waterwayForm.querySelector("button");
  await withBusy(button, async () => {
    const chamber = document.querySelector("#ww-chamber").value;
    try {
      const decision = await api(`/api/chambers/${chamber}/waterway/request`, {
        method: "POST", body: JSON.stringify({ direction: document.querySelector("#ww-direction").value }),
      });
      const occ = decision.occupancy;
      setMessage(waterwayMessage, `Cleared: ${occ.convoy} heads ${occ.direction} through ${occ.waterway_id}.`);
      await refresh();
    } catch (error) {
      setMessage(waterwayMessage, error.message);
      await refresh();
    }
  });
});

refresh().catch((error) => setMessage(message, error.message));
setInterval(() => refresh().catch(() => {}), 5000);
