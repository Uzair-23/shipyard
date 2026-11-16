// Prints what the platform lets an app see about a request: the identity
// headers Shipyard sets, every header and cookie received, and any DEMO_*
// environment variables (to show that env vars and secrets reach the app).
const http = require("http");

const port = Number(process.env.PORT) || 8080;

function parseCookies(header) {
  const cookies = {};
  for (const part of (header || "").split(";")) {
    const i = part.indexOf("=");
    if (i > 0) cookies[part.slice(0, i).trim()] = part.slice(i + 1).trim();
  }
  return cookies;
}

http
  .createServer((req, res) => {
    const env = {};
    for (const [k, v] of Object.entries(process.env)) {
      if (k.startsWith("DEMO_")) env[k] = v;
    }
    res.writeHead(200, { "Content-Type": "application/json; charset=utf-8" });
    res.end(
      JSON.stringify(
        {
          user_email: req.headers["x-shipyard-user-email"] || null,
          user_id: req.headers["x-shipyard-user-id"] || null,
          cookies: parseCookies(req.headers.cookie),
          headers: req.headers,
          env,
        },
        null,
        2,
      ) + "\n",
    );
  })
  .listen(port, "0.0.0.0");
