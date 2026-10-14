// Polls the app's own endpoints so the page shows something that actually comes
// from the running container rather than being baked into the HTML.
const els = {
  dot: document.getElementById("status-dot"),
  hostname: document.getElementById("hostname"),
  uptime: document.getElementById("uptime"),
  node: document.getElementById("node"),
  platform: document.getElementById("platform"),
  updated: document.getElementById("updated"),
  refresh: document.getElementById("refresh"),
};

function describeUptime(seconds) {
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${seconds % 60}s`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}

let startedAt = null;

async function load() {
  els.updated.textContent = "checking\u2026";
  try {
    const [health, info] = await Promise.all([
      fetch("/health").then((r) => r.json()),
      fetch("/api/info").then((r) => r.json()),
    ]);

    els.dot.classList.remove("down");
    els.dot.classList.add("up");

    els.hostname.textContent = info.hostname;
    els.node.textContent = info.node;
    els.platform.textContent = info.platform;
    if (startedAt === null) startedAt = health.uptimeSeconds;
    els.uptime.textContent = describeUptime(health.uptimeSeconds);
    els.updated.textContent = `updated ${new Date().toLocaleTimeString()}`;
  } catch (err) {
    els.dot.classList.remove("up");
    els.dot.classList.add("down");
    els.updated.textContent = `unreachable: ${err.message}`;
  }
}

els.refresh.addEventListener("click", load);
load();
// Keep the uptime ticking without hammering the server.
setInterval(load, 10000);
