import assert from "node:assert/strict";
import { Sandbox } from "../sdk/dist/index.js";

const sandbox = await Sandbox.get({ name: process.env.SANDBOX_SMOKE_NAME ?? "sdk-e2e" });
const script = `
set -euo pipefail
rm -rf vite-app
pnpm create vite@latest vite-app --template react
cd vite-app
pnpm install --frozen-lockfile=false
pnpm add tailwindcss @tailwindcss/vite
pnpm exec vite build
test -f dist/index.html
`;
const command = await sandbox.runCommand({
  cmd: "bash",
  args: ["-lc", script],
  timeoutMs: 180_000,
});
if (command.exitCode !== 0) {
  process.stderr.write(await command.stderr());
}
assert.equal(command.exitCode, 0);
console.log(JSON.stringify({ workload: "vite-react-tailwind", build: "ok" }));
