import assert from "node:assert/strict";
import { Sandbox } from "../sdk/dist/index.js";

const name = `policy-e2e-${Date.now()}`;
const sandbox = await Sandbox.create({ name, runtime: "node24", resources: { vcpus: 1 }, timeout: 300_000 });
try {
  const command = await sandbox.runCommand({
    cmd: "bash",
    args: ["-lc", `
set -euo pipefail
for host in appleid.cdn-apple.com js.stripe.com dashboard.convex.dev api.convex.dev console.anthropic.com claude.ai; do
  code=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "https://$host/")
  test "$code" != 000
done
! curl -fsS --max-time 4 https://sentry.io >/dev/null 2>&1
! curl -fsS --max-time 4 https://www.google.com/search?q=x >/dev/null 2>&1
`],
    timeoutMs: 90_000,
  });
  if (command.exitCode !== 0) process.stderr.write(await command.stderr());
  assert.equal(command.exitCode, 0);
  console.log(JSON.stringify({ approvedServices: "reachable", unapprovedServices: "blocked" }));
} finally {
  await sandbox.delete();
}
