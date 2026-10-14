// A minimal node web server, used to exercise the deploy pipeline end to end.
//
// It listens on 0.0.0.0 (not localhost) because it runs inside a container: the
// port has to be reachable from outside it for the URL the deploy reports to
// work.
const http = require("http");
const os = require("os");

const PORT = Number(process.env.PORT || 3000);
const startedAt = new Date();

const server = http.createServer((req, res) => {
  if (req.url === "/health") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ ok: true, uptimeSeconds: (Date.now() - startedAt) / 1000 }));
    return;
  }

  res.writeHead(200, { "Content-Type": "text/plain" });
  res.end(`hello from ${os.hostname()}\nstarted at ${startedAt.toISOString()}\n`);
});

server.listen(PORT, "0.0.0.0", () => {
  console.log(`sample server listening on 0.0.0.0:${PORT}`);
});
