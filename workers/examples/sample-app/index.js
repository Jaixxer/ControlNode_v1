// A small static frontend served over HTTP, used to exercise the deploy
// pipeline end to end.
//
// It listens on 0.0.0.0 (not localhost) because it runs inside a container: the
// port has to be reachable from outside it for the URL the deploy reports to
// work. It deliberately has no dependencies, so the image builds quickly and
// without reaching a package registry.
const http = require("http");
const fs = require("fs");
const os = require("os");
const path = require("path");

const PORT = Number(process.env.PORT || 3000);
const PUBLIC_DIR = path.join(__dirname, "public");
const startedAt = new Date();

const MIME_TYPES = {
  ".html": "text/html; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".svg": "image/svg+xml",
  ".json": "application/json",
};

function sendJSON(res, status, body) {
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(JSON.stringify(body));
}

function serveStatic(req, res) {
  // Resolve inside PUBLIC_DIR so a crafted path cannot escape it.
  const requested = req.url === "/" ? "/index.html" : req.url.split("?")[0];
  const resolved = path.join(PUBLIC_DIR, path.normalize(requested));
  if (!resolved.startsWith(PUBLIC_DIR)) {
    res.writeHead(403).end("forbidden");
    return;
  }

  fs.readFile(resolved, (err, data) => {
    if (err) {
      res.writeHead(404, { "Content-Type": "text/plain" });
      res.end("not found");
      return;
    }
    res.writeHead(200, {
      "Content-Type": MIME_TYPES[path.extname(resolved)] || "application/octet-stream",
    });
    res.end(data);
  });
}

const server = http.createServer((req, res) => {
  if (req.url.startsWith("/health")) {
    sendJSON(res, 200, {
      ok: true,
      hostname: os.hostname(),
      uptimeSeconds: Math.round((Date.now() - startedAt) / 1000),
    });
    return;
  }

  if (req.url.startsWith("/api/info")) {
    sendJSON(res, 200, {
      hostname: os.hostname(),
      platform: `${os.platform()} ${os.release()}`,
      node: process.version,
      startedAt: startedAt.toISOString(),
    });
    return;
  }

  serveStatic(req, res);
});

server.listen(PORT, "0.0.0.0", () => {
  console.log(`sample frontend listening on 0.0.0.0:${PORT}`);
});
