const http = require("http");

const port = Number(process.env.PORT) || 8080;

http
  .createServer((req, res) => {
    res.writeHead(200, { "Content-Type": "text/plain; charset=utf-8" });
    res.end("Hello from Shipyard (node)\n");
  })
  .listen(port, "0.0.0.0");
