import assert from "node:assert/strict";
import { Sandbox } from "../sdk/dist/index.js";

const sandbox = await Sandbox.get({ name: process.env.SANDBOX_SMOKE_NAME ?? "sdk-e2e" });
const command = await sandbox.runCommand({
  cmd: "bash",
  args: ["-lc", `
set -euo pipefail
cd vite-app
pnpm add convex stripe @stripe/stripe-js @anthropic-ai/claude-code
pnpm exec convex --version
pnpm exec claude --version
pnpm exec vite build
`],
  timeoutMs: 180_000,
});
if (command.exitCode !== 0) process.stderr.write(await command.stderr());
assert.equal(command.exitCode, 0);
console.log(JSON.stringify({ convex: "installed", stripe: "installed", claudeCode: "installed", viteBuild: "ok" }));
