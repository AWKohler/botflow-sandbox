import assert from "node:assert/strict";
import { Sandbox } from "../sdk/dist/index.js";

const sandbox = await Sandbox.get({ name: process.env.SANDBOX_SMOKE_NAME ?? "sdk-e2e" });
const normal = await sandbox.runCommand({
  cmd: "bash",
  args: ["-lc", `
set -euo pipefail
test "$(id -u)" = 1000
test ! -e /dev/kvm
test ! -e /run/sandbox-host/sandboxd.sock
! curl -kfsS --max-time 4 https://1.1.1.1 >/dev/null 2>&1
! curl -fsS --max-time 4 http://169.254.169.254/latest/meta-data/ >/dev/null 2>&1
! curl -fsS --max-time 4 http://100.93.37.55:22 >/dev/null 2>&1
! ping -c 1 -W 2 1.1.1.1 >/dev/null 2>&1
dig +time=2 +tries=1 @8.8.8.8 example.com | grep -q 'status: REFUSED'
`],
  timeoutMs: 60_000,
});
if (normal.exitCode !== 0) process.stderr.write(await normal.stderr());
assert.equal(normal.exitCode, 0);

const root = await sandbox.runCommand({
  cmd: "bash",
  args: ["-lc", `
set -euo pipefail
test "$(id -u)" = 0
test "$(cat /etc/hostname)" = sandbox-host
test ! -e /dev/kvm
test ! -e /dev/sda
awk '/MemTotal/ { if ($2 < 1800000 || $2 > 2300000) exit 1 }' /proc/meminfo
`],
  sudo: true,
});
if (root.exitCode !== 0) process.stderr.write(await root.stderr());
assert.equal(root.exitCode, 0);
console.log(JSON.stringify({ isolation: "ok", guestRoot: "contained", memoryMiB: 2048 }));
