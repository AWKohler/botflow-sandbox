import assert from "node:assert/strict";
import { Sandbox } from "../sdk/dist/index.js";

const name = process.env.SANDBOX_SMOKE_NAME ?? "sdk-e2e";
let sandbox;
try {
  sandbox = await Sandbox.create({
    name,
    runtime: "node24",
    resources: { vcpus: 1 },
    timeout: 600_000,
    ports: [4173],
  });
} catch (error) {
  if (error?.json?.error?.code !== "sandbox_exists") throw error;
  sandbox = await Sandbox.get({ name });
}

await sandbox.writeFiles([
  { path: "sdk-smoke.txt", content: "sdk-write-ok\n", mode: 0o640 },
  {
    path: "server.mjs",
    content: "import http from 'node:http'; http.createServer((_, res) => res.end('preview-ok\\n')).listen(4173, '0.0.0.0');\n",
    mode: 0o640,
  },
]);
const written = await sandbox.readFileToBuffer({ path: "sdk-smoke.txt" });
assert.equal(written?.toString(), "sdk-write-ok\n");

const command = await sandbox.runCommand({
  cmd: "node",
  args: ["-e", "process.stdout.write(require('fs').readFileSync('sdk-smoke.txt'))"],
});
assert.equal(command.exitCode, 0);
assert.equal(await command.stdout(), "sdk-write-ok\n");

await sandbox.runCommand({ cmd: "node", args: ["server.mjs"], detached: true });
const previewURL = sandbox.domain(4173);
let preview;
for (let attempt = 0; attempt < 20; attempt++) {
  try {
    preview = await fetch(previewURL).then((response) => response.text());
    break;
  } catch {
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
}
assert.equal(preview, "preview-ok\n");

const snapshot = await sandbox.snapshot();
assert.ok(snapshot.snapshotId);
sandbox = await Sandbox.get({ name });
const restored = await sandbox.readFileToBuffer({ path: "sdk-smoke.txt" });
assert.equal(restored?.toString(), "sdk-write-ok\n");

console.log(JSON.stringify({
  sdk: "ok",
  sandbox: sandbox.name,
  session: sandbox.sessionId,
  snapshot: snapshot.snapshotId,
  restored: true,
  preview: previewURL,
}));
