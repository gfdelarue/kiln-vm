import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { before, test } from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");
let contract;
const temporaryRoots = new Set();

process.on("exit", () => {
  for (const root of temporaryRoots) {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

before(() => {
  contract = readContract();
});

test("static package contract keeps safe defaults, service supervision and licenses", verifyStaticPackageContract);
test("maintainer scripts are valid POSIX shell", verifyMaintainerScriptSyntax);
test("fresh install writes safe defaults", verifyFreshInstallUsesSafeDefaults);
test("existing config without a listener gets the safe default", verifyExistingConfigWithoutListenerGetsSafeDefault);
test("existing listeners are preserved", verifyExistingListenersArePreserved);
test("explicit insecure remote HTTP opt-in is preserved", verifyExplicitServerOptInIsPreserved);
test("effective listener assignment order is preserved", verifyEffectiveAssignmentOrderingIsPreserved);
test("atomic update preserves metadata and removes temporary files", verifyAtomicUpdatePreservesMetadataAndCleansTemps);
test("failed update removes temporary files and keeps the config", verifyFailureCleansAtomicTemps);
test("profile token discovery fills an empty token setting", verifyProfileTokenFillsEmptySetting);
test("non-regular operator config is preserved", verifyNonRegularOperatorConfigIsPreserved);
test("KILN_VM_DEB_ROOT never touches live services", verifyDebRootCannotTouchLiveServices);
test("service activation follows Debian helper semantics", verifyServiceActivationContract);
test("documentation describes the management transport modes", verifyDocumentationContract);

function readContract() {
  const result = spawnSync(process.execPath, ["scripts/build-deb.mjs", "--print-deb-contract"], {
    cwd: repoRoot,
    encoding: "utf8",
  });
  if (result.status !== 0) {
    throw new Error(`build-deb contract failed:\n${result.stderr || result.stdout}`);
  }
  return JSON.parse(result.stdout);
}

function verifyStaticPackageContract() {
  assert.ok(contract.data_files.includes("/usr/share/kiln-vm/kiln-vmd.env.defaults"));
  assert.ok(contract.data_files.includes("/usr/share/doc/kiln-vm/copyright"));
  assert.match(contract.copyright, /^Files: \*\nCopyright: George de la Rue\nLicense: Apache-2\.0$/m);
  assert.match(contract.copyright, /\/usr\/share\/common-licenses\/Apache-2\.0/);
  assert.ok(contract.data_files.includes("/usr/share/doc/kiln-vm/third-party/miekg-dns.LICENSE"));
  assert.ok(contract.data_files.includes("/usr/share/doc/kiln-vm/third-party/golang-x-net.LICENSE"));
  assert.ok(contract.omitted_data_files.includes("/etc/kiln-vm/kiln-vmd.env"));
  assert.ok(contract.dependencies.includes("init-system-helpers"));
  for (const windowsOnly of ["libvirt-daemon-system", "virtinst", "ovmf", "swtpm", "openssh-server"]) {
    assert.ok(!contract.dependencies.includes(windowsOnly), `${windowsOnly} must not be a hard dependency`);
  }
  for (const windowsTool of ["libvirt-daemon-system", "virtinst", "ovmf", "swtpm", "mtools"]) {
    assert.ok(contract.recommends.includes(windowsTool), `${windowsTool} must be recommended`);
  }
  assert.match(contract.daemon_env_defaults, /^KILN_VM_LIBVIRT_CIDR=10\.232\.0\.0\/24$/m);

  assert.match(contract.daemon_env_defaults, /^KILN_VM_LISTEN_ADDR=127\.0\.0\.1:5456$/m);
  assert.match(contract.daemon_env_defaults, /^KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=false$/m);
  assert.match(contract.daemon_env_defaults, /^KILN_VM_TLS_ENABLED=false$/m);
  assert.match(contract.daemon_env_defaults, /^KILN_VM_TLS_DOMAIN=$/m);
  assert.match(contract.daemon_env_defaults, /^KILN_VM_TLS_CERT_DIR=\/var\/lib\/kiln-vm\/certs$/m);
  assert.match(contract.daemon_env_defaults, /^KILN_VM_TLS_LISTEN_ADDR=:443$/m);
  assert.match(contract.daemon_env_defaults, /^KILN_VM_ACME_LISTEN_ADDR=:80$/m);
  assert.match(contract.daemon_env_defaults, /^KILN_VM_ACME_EMAIL=$/m);
  assert.doesNotMatch(contract.daemon_env_defaults, /^KILN_VM_LISTEN_ADDR=:8080$/m);
  assert.doesNotMatch(contract.daemon_env_defaults, /^KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=true$/m);
  assert.match(contract.daemon_env_defaults, /^KILN_VM_PROFILE_IMAGE_CACHE_MAX_MB=8192$/m);
  assert.match(contract.daemon_env_defaults, /^KILN_VM_PROFILE_IMAGE_TOKEN_FILE=$/m);
  assert.match(contract.daemon_env_defaults, /^KILN_VM_GUEST_DNS_PROBE_NAME=example\.com\.$/m);

  assert.match(contract.daemon_service_unit, /^Restart=always$/m);
  assert.match(contract.daemon_service_unit, /^RestartPreventExitStatus=78$/m);
  assert.match(contract.daemon_service_unit, /^Type=notify$/m);
  assert.match(contract.daemon_service_unit, /^NotifyAccess=main$/m);
  assert.match(contract.postinst_service_activation, /deb-systemd-invoke (?:start|restart) kiln-vmd\.service/);
  assert.doesNotMatch(contract.postinst_service_activation, /systemctl[^\n]*(?:start|restart)[^\n]*kiln-vmd/);
  assert.match(contract.prerm, /deb-systemd-invoke stop kiln-vmd\.service/);
  assert.doesNotMatch(contract.prerm, /systemctl[^\n]*stop[^\n]*kiln-vmd/);

  const miekgDNSLicense = fs.readFileSync(
    path.join(repoRoot, "third_party", "github.com", "miekg", "dns", "LICENSE"),
    "utf8",
  );
  assert.match(miekgDNSLicense, /^BSD 3-Clause License$/m);
  assert.match(
    miekgDNSLicense,
    /^Copyright \(c\) 2009, The Go Authors\. Extensions copyright \(c\) 2011, Miek Gieben\.$/m,
  );

  const golangXNetLicense = fs.readFileSync(
    path.join(repoRoot, "third_party", "golang.org", "x", "net", "LICENSE"),
    "utf8",
  );
  assert.match(golangXNetLicense, /^Copyright 2009 The Go Authors\.$/m);
  assert.match(golangXNetLicense, /Redistribution and use in source and binary forms/);
}

function verifyMaintainerScriptSyntax() {
  withRoot((root) => {
    for (const [name, body] of [
      ["postinst", contract.postinst],
      ["postinst-service-activation", contract.postinst_service_activation],
      ["prerm", contract.prerm],
    ]) {
      const script = path.join(root, name);
      fs.writeFileSync(script, body, { mode: 0o755 });
      const result = spawnSync("/bin/sh", ["-n", script], { encoding: "utf8" });
      assert.equal(result.status, 0, `${name} is not valid POSIX shell:\n${result.stderr}`);
    }
  });
}

async function verifyFreshInstallUsesSafeDefaults(t) {
  await t.test("without a profile token file", () => withRoot((root) => {
    writeDefaults(root);
    runPostinst(root);

    const env = readEnv(root);
    assert.match(env, /^KILN_VM_LISTEN_ADDR=127\.0\.0\.1:5456$/m);
    assert.match(env, /^KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=false$/m);
    assert.match(env, /^KILN_VM_TLS_ENABLED=false$/m);
    assert.match(env, /^KILN_VM_TLS_LISTEN_ADDR=:443$/m);
    assert.match(env, /^KILN_VM_ACME_LISTEN_ADDR=:80$/m);
    assert.doesNotMatch(env, /^KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=true$/m);
    assert.match(env, /^KILN_VM_PROFILE_IMAGE_CACHE_MAX_MB=8192$/m);
    assert.match(env, /^KILN_VM_PROFILE_IMAGE_TOKEN_FILE=$/m);
    assert.equal(fs.statSync(envPath(root)).mode & 0o777, 0o640);
  }));

  await t.test("with a profile token file", () => withRoot((root) => {
    writeDefaults(root);
    fs.mkdirSync(path.join(root, "etc", "kiln-vm"), { recursive: true });
    fs.writeFileSync(path.join(root, "etc", "kiln-vm", "profile-image-token"), "secret\n", { mode: 0o600 });
    runPostinst(root);
    assert.match(readEnv(root), /^KILN_VM_PROFILE_IMAGE_TOKEN_FILE=\/etc\/kiln-vm\/profile-image-token$/m);
  }));
}

function verifyExistingConfigWithoutListenerGetsSafeDefault() {
  withRoot((root) => {
    writeDefaults(root);
    writeEnv(root, "# operator configuration\nKILN_VM_CUSTOM_SETTING=preserved\n");
    runPostinst(root, { args: ["configure", "0.1.115"] });
    const env = readEnv(root);
    assert.match(env, /^# operator configuration$/m);
    assert.match(env, /^KILN_VM_CUSTOM_SETTING=preserved$/m);
    assert.match(env, /^KILN_VM_LISTEN_ADDR=127\.0\.0\.1:5456$/m);
    assert.match(env, /^KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=false$/m);
    assert.equal(assignmentCount(env, "KILN_VM_LISTEN_ADDR"), 1);
  });
}

async function verifyExistingListenersArePreserved(t) {
  const cases = [
    ["quoted value", "KILN_VM_LISTEN_ADDR=\":8080\"\n"],
    ["leading whitespace", " KILN_VM_LISTEN_ADDR=:8080\n"],
    ["whitespace before equals", "KILN_VM_LISTEN_ADDR =:8080\n"],
    ["trailing whitespace", "KILN_VM_LISTEN_ADDR=:8080 \n"],
    ["duplicate assignments", "KILN_VM_LISTEN_ADDR=:8080\nKILN_VM_LISTEN_ADDR=:8080\n"],
    ["competing listener", "KILN_VM_LISTEN_ADDR=:8080\nKILN_VM_LISTEN_ADDR=10.0.0.8:8080\n"],
    ["custom remote listener", "KILN_VM_LISTEN_ADDR=10.0.0.8:8080\n"],
    ["custom listener without terminal newline", "KILN_VM_LISTEN_ADDR=10.0.0.9:8080"],
    ["custom loopback listener", "KILN_VM_LISTEN_ADDR=127.0.0.1:18080\n"],
  ];

  for (const [name, input] of cases) {
    await t.test(name, () => withRoot((root) => {
      writeDefaults(root);
      writeEnv(root, input);
      runPostinst(root, { args: ["configure", "0.1.115"] });
      const env = readEnv(root);
      assert.ok(env.startsWith(input), `${name} was unexpectedly rewritten`);
      assert.match(env, /^KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=false$/m, `${name} did not receive the safe opt-in default`);
      assert.doesNotMatch(env, /^KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=true$/m, `${name} synthesized an insecure opt-in`);
    }));
  }
}

async function verifyExplicitServerOptInIsPreserved(t) {
  for (const value of ["true", "false", ""]) {
    await t.test(`KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=${value}`, () => withRoot((root) => {
      writeDefaults(root);
      const input = `KILN_VM_LISTEN_ADDR=:8080\nKILN_VM_ALLOW_INSECURE_REMOTE_HTTP=${value}\n`;
      writeEnv(root, input);
      runPostinst(root, { args: ["configure", "0.1.115"] });
      const env = readEnv(root);
      assert.ok(env.startsWith(input), `explicit opt-in value ${JSON.stringify(value)} was not preserved`);
      assert.match(env, /^KILN_VM_LISTEN_ADDR=:8080$/m);
      assert.equal(assignmentCount(env, "KILN_VM_ALLOW_INSECURE_REMOTE_HTTP"), 1);
      assert.match(env, new RegExp(`^KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=${value}$`, "m"));
    }));
  }
}

function verifyEffectiveAssignmentOrderingIsPreserved() {
  withRoot((root) => {
    writeDefaults(root);
    const listenerLines = [
      "KILN_VM_LISTEN_ADDR=127.0.0.1:18080",
      "KILN_VM_LISTEN_ADDR=10.0.0.8:8080",
    ];
    writeEnv(root, `${listenerLines[0]}\nKILN_VM_CUSTOM_SETTING=middle\n${listenerLines[1]}\n`);
    runPostinst(root, { args: ["configure", "0.1.115"] });
    const effectiveLines = readEnv(root).split("\n").filter((line) => line.startsWith("KILN_VM_LISTEN_ADDR="));
    assert.deepEqual(effectiveLines, listenerLines);
  });
}

function verifyAtomicUpdatePreservesMetadataAndCleansTemps() {
  withRoot((root) => {
    writeDefaults(root);
    writeEnv(root, "# owner data\nKILN_VM_LISTEN_ADDR=:8080\nKILN_VM_CUSTOM_SETTING=preserved\n", 0o600);
    const before = fs.statSync(envPath(root));

    runPostinst(root, { args: ["configure", "0.1.115"] });

    const after = fs.statSync(envPath(root));
    assert.notEqual(after.ino, before.ino, "changed environment was not replaced atomically");
    assert.equal(after.mode & 0o777, before.mode & 0o777);
    assert.equal(after.uid, before.uid);
    assert.equal(after.gid, before.gid);
    assert.match(readEnv(root), /^# owner data$/m);
    assert.match(readEnv(root), /^KILN_VM_CUSTOM_SETTING=preserved$/m);
    assertNoEnvironmentTemps(root);
  });
}

function verifyFailureCleansAtomicTemps() {
  withRoot((root) => {
    writeDefaults(root);
    writeEnv(root, "KILN_VM_LISTEN_ADDR=:8080\n");
    const fakeBin = path.join(root, "fake-bin");
    fs.mkdirSync(fakeBin, { recursive: true });
    writeExecutable(path.join(fakeBin, "cp"), "#!/bin/sh\nexit 19\n");

    const result = runPostinst(root, {
      env: { PATH: `${fakeBin}:${process.env.PATH}` },
      expectedStatus: 19,
    });
    assert.equal(result.status, 19);
    assertNoEnvironmentTemps(root);
    assert.equal(readEnv(root), "KILN_VM_LISTEN_ADDR=:8080\n");
  });
}

function verifyProfileTokenFillsEmptySetting() {
  withRoot((root) => {
    writeDefaults(root);
    writeEnv(root, [
      "KILN_VM_LISTEN_ADDR=127.0.0.1:18080",
      "KILN_VM_PROFILE_IMAGE_TOKEN_FILE=",
      "KILN_VM_CUSTOM_SETTING=preserved",
      "",
    ].join("\n"));
    fs.writeFileSync(path.join(root, "etc", "kiln-vm", "profile-image-token"), "secret\n", { mode: 0o600 });

    runPostinst(root, { args: ["configure", "0.1.115"] });

    const env = readEnv(root);
    assert.match(env, /^KILN_VM_LISTEN_ADDR=127\.0\.0\.1:18080$/m);
    assert.equal(assignmentCount(env, "KILN_VM_LISTEN_ADDR"), 1);
    assert.match(env, /^KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=false$/m);
    assert.match(env, /^KILN_VM_PROFILE_IMAGE_TOKEN_FILE=\/etc\/kiln-vm\/profile-image-token$/m);
    assert.match(env, /^KILN_VM_CUSTOM_SETTING=preserved$/m);
    assert.equal(assignmentCount(env, "KILN_VM_PROFILE_IMAGE_TOKEN_FILE"), 1);
    assertNoEnvironmentTemps(root);
  });
}

async function verifyNonRegularOperatorConfigIsPreserved(t) {
  await t.test("symlink to an operator file", () => withRoot((root) => {
    writeDefaults(root);
    const etcDir = path.join(root, "etc", "kiln-vm");
    fs.mkdirSync(etcDir, { recursive: true });
    const target = path.join(etcDir, "operator.env");
    const operatorEnv = "KILN_VM_LISTEN_ADDR=10.0.0.8:8080\nKILN_VM_CUSTOM_SETTING=preserved\n";
    fs.writeFileSync(target, operatorEnv);
    fs.symlinkSync("operator.env", envPath(root));

    runPostinst(root);

    assert.equal(fs.readlinkSync(envPath(root)), "operator.env");
    assert.equal(fs.readFileSync(target, "utf8"), operatorEnv);
  }));

  await t.test("dangling symlink", () => withRoot((root) => {
    writeDefaults(root);
    fs.mkdirSync(path.dirname(envPath(root)), { recursive: true });
    fs.symlinkSync("missing-operator.env", envPath(root));
    runPostinst(root);
    assert.equal(fs.readlinkSync(envPath(root)), "missing-operator.env");
    assert.equal(fs.existsSync(path.join(path.dirname(envPath(root)), "missing-operator.env")), false);
  }));
}

function verifyDebRootCannotTouchLiveServices() {
  withRoot((root) => {
    writeDefaults(root);
    const { binDir, callLog } = createServiceFakes(root);
    runPostinst(root, { env: { PATH: `${binDir}:${process.env.PATH}` } });
    runPrerm(root, { env: { PATH: `${binDir}:${process.env.PATH}` }, args: ["remove"] });
    assert.equal(readOptionalFile(callLog), "", "KILN_VM_DEB_ROOT invoked a live service helper");
  });
}

async function verifyServiceActivationContract(t) {
  await t.test("fresh install enables and starts kiln-vmd", () => withRoot((root) => {
    const { binDir, callLog } = createServiceFakes(root);
    const runDir = path.join(root, "run", "systemd", "system");
    fs.mkdirSync(runDir, { recursive: true });
    runServiceActivation(root, {
      args: ["configure"],
      env: serviceEnvironment(binDir, callLog, runDir),
    });
    const calls = readOptionalFile(callLog);
    assert.match(calls, /^deb-systemd-helper enable kiln-vmd\.service$/m);
    assert.match(calls, /^deb-systemd-helper update-state kiln-vmd\.service$/m);
    assert.match(calls, /^systemctl --system daemon-reload$/m);
    assert.match(calls, /^deb-systemd-invoke start kiln-vmd\.service$/m);
    assert.doesNotMatch(calls, /^deb-systemd-invoke restart kiln-vmd\.service$/m);
  }));

  await t.test("upgrade of an enabled unit restarts kiln-vmd", () => withRoot((root) => {
    const { binDir, callLog } = createServiceFakes(root);
    const runDir = path.join(root, "run", "systemd", "system");
    fs.mkdirSync(runDir, { recursive: true });
    runServiceActivation(root, {
      args: ["configure", "0.1.115"],
      env: {
        ...serviceEnvironment(binDir, callLog, runDir),
        KILN_VM_FAKE_INSTALLED_STATUS: "0",
        KILN_VM_FAKE_WAS_ENABLED_STATUS: "0",
      },
    });
    const calls = readOptionalFile(callLog);
    assert.match(calls, /^deb-systemd-helper unmask kiln-vmd\.service$/m);
    assert.match(calls, /^deb-systemd-helper enable kiln-vmd\.service$/m);
    assert.match(calls, /^deb-systemd-helper update-state kiln-vmd\.service$/m);
    assert.match(calls, /^deb-systemd-invoke restart kiln-vmd\.service$/m);
  }));

  await t.test("no running systemd leaves the service alone", () => withRoot((root) => {
    const { binDir, callLog } = createServiceFakes(root);
    const absentRunDir = path.join(root, "not-running");
    runServiceActivation(root, {
      env: serviceEnvironment(binDir, callLog, absentRunDir),
    });
    const calls = readOptionalFile(callLog);
    assert.doesNotMatch(calls, /^systemctl /m);
    assert.doesNotMatch(calls, /^deb-systemd-invoke /m);
  }));

  await t.test("chroot leaves the service alone", () => withRoot((root) => {
    const { binDir, callLog } = createServiceFakes(root);
    const runDir = path.join(root, "run", "systemd", "system");
    fs.mkdirSync(runDir, { recursive: true });
    runServiceActivation(root, {
      env: {
        ...serviceEnvironment(binDir, callLog, runDir),
        KILN_VM_FAKE_ISCHROOT_STATUS: "0",
      },
    });
    const calls = readOptionalFile(callLog);
    assert.doesNotMatch(calls, /^systemctl /m);
    assert.doesNotMatch(calls, /^deb-systemd-invoke /m);
  }));

  await t.test("daemon-reload failure stops before invoke", () => withRoot((root) => {
    const { binDir, callLog } = createServiceFakes(root);
    const runDir = path.join(root, "run", "systemd", "system");
    fs.mkdirSync(runDir, { recursive: true });
    const result = runServiceActivation(root, {
      env: {
        ...serviceEnvironment(binDir, callLog, runDir),
        KILN_VM_FAKE_SYSTEMCTL_STATUS: "17",
      },
      expectedStatus: 17,
    });
    assert.equal(result.status, 17);
    assert.doesNotMatch(readOptionalFile(callLog), /^deb-systemd-invoke /m);
  }));

  await t.test("invoke failure propagates its status", () => withRoot((root) => {
    const { binDir, callLog } = createServiceFakes(root);
    const runDir = path.join(root, "run", "systemd", "system");
    fs.mkdirSync(runDir, { recursive: true });
    const result = runServiceActivation(root, {
      env: {
        ...serviceEnvironment(binDir, callLog, runDir),
        KILN_VM_FAKE_INVOKE_STATUS: "23",
      },
      expectedStatus: 23,
    });
    assert.equal(result.status, 23);
  }));

  await t.test("missing systemctl fails", () => withRoot((root) => {
    const { binDir, callLog } = createServiceFakes(root);
    const runDir = path.join(root, "run", "systemd", "system");
    fs.mkdirSync(runDir, { recursive: true });
    fs.rmSync(path.join(binDir, "systemctl"));
    const result = runServiceActivation(root, {
      env: {
        ...serviceEnvironment(binDir, callLog, runDir),
        PATH: binDir,
      },
      expectedStatus: 1,
    });
    assert.equal(result.status, 1);
    assert.match(result.stderr, /systemctl is unavailable/);
  }));

  await t.test("missing deb-systemd-invoke fails", () => withRoot((root) => {
    const { binDir, callLog } = createServiceFakes(root);
    const runDir = path.join(root, "run", "systemd", "system");
    fs.mkdirSync(runDir, { recursive: true });
    fs.rmSync(path.join(binDir, "deb-systemd-invoke"));
    const result = runServiceActivation(root, {
      env: {
        ...serviceEnvironment(binDir, callLog, runDir),
        PATH: binDir,
      },
      expectedStatus: 1,
    });
    assert.equal(result.status, 1);
    assert.match(result.stderr, /deb-systemd-invoke is unavailable/);
  }));
}

function verifyDocumentationContract() {
  const read = (relativePath) => fs.readFileSync(path.join(repoRoot, relativePath), "utf8");
  const readme = read("README.md");
  const tls = read("docs/tls-setup.md");
  const operator = read("docs/operator-guide.md");
  const combined = [readme, tls, operator].join("\n");

  assert.match(readme, /^KILN_VM_LISTEN_ADDR=127\.0\.0\.1:5456$/m);
  assert.match(readme, /^KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=false$/m);
  assert.doesNotMatch(readme, /^KILN_VM_LISTEN_ADDR=:8080$/m);
  assert.match(tls, /exactly\s+three\s+supported\s+management\s+transport\s+modes/i);
  assert.match(tls, /loopback HTTP/i);
  assert.match(tls, /remote HTTPS/i);
  assert.match(tls, /explicit insecure remote HTTP/i);
  assert.match(tls, /does\s+not\s+provide\s+application-layer\s+encryption/i);
  assert.match(tls, /cannot\s+share\s+them/i);
  assert.match(operator, /RestartPreventExitStatus=78/);
  assert.match(operator, /leaves\s+every\s+existing\s+line\s+alone/i);
  assert.match(operator, /never\s+adds\s+`KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=true`/);
  assert.match(combined, /KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=true/);
  assert.match(combined, /KILN_VM_ALLOW_INSECURE_HTTP=true/);
  assert.match(combined, /kiln-vm login[^\n]*--allow-insecure-http/);
}

function makeRoot() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "kiln-vm-deb-contract-"));
  temporaryRoots.add(root);
  return root;
}

function withRoot(callback) {
  const root = makeRoot();
  try {
    callback(root);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
    temporaryRoots.delete(root);
  }
}

function writeDefaults(root) {
  const target = path.join(root, "usr", "share", "kiln-vm", "kiln-vmd.env.defaults");
  fs.mkdirSync(path.dirname(target), { recursive: true });
  fs.writeFileSync(target, contract.daemon_env_defaults);
}

function envPath(root) {
  return path.join(root, "etc", "kiln-vm", "kiln-vmd.env");
}

function writeEnv(root, content, mode = 0o640) {
  const target = envPath(root);
  fs.mkdirSync(path.dirname(target), { recursive: true });
  fs.writeFileSync(target, content, { mode });
  fs.chmodSync(target, mode);
}

function readEnv(root) {
  return fs.readFileSync(envPath(root), "utf8");
}

function assignmentCount(content, key) {
  return content.split("\n").filter((line) => new RegExp(`^\\s*${key}\\s*=`).test(line)).length;
}

function assertNoEnvironmentTemps(root) {
  const entries = fs.readdirSync(path.join(root, "etc", "kiln-vm"));
  assert.deepEqual(entries.filter((entry) => /^kiln-vmd\.env\.(?:content|replacement|next)\./.test(entry)), []);
}

function runPostinst(root, options = {}) {
  return runMaintainerScript(root, "postinst", contract.postinst, {
    ...options,
    env: { ...options.env, KILN_VM_DEB_ROOT: root },
  });
}

function runPrerm(root, options = {}) {
  return runMaintainerScript(root, "prerm", contract.prerm, {
    ...options,
    env: { ...options.env, KILN_VM_DEB_ROOT: root },
  });
}

function runServiceActivation(root, options = {}) {
  return runMaintainerScript(root, "postinst-service-activation", contract.postinst_service_activation, options);
}

function runMaintainerScript(root, name, body, { args = [], env = {}, expectedStatus = 0 } = {}) {
  const script = path.join(root, name);
  fs.writeFileSync(script, body, { mode: 0o755 });
  const result = spawnSync("/bin/sh", [script, ...args], {
    cwd: repoRoot,
    env: { ...process.env, ...env },
    encoding: "utf8",
  });
  assert.equal(
    result.status,
    expectedStatus,
    `${name} exited ${result.status}, expected ${expectedStatus}:\nstdout:\n${result.stdout}\nstderr:\n${result.stderr}`,
  );
  return result;
}

function createServiceFakes(root) {
  const binDir = path.join(root, "service-bin");
  const callLog = path.join(root, "service-calls.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeExecutable(path.join(binDir, "ischroot"), `#!/bin/sh
exit "\${KILN_VM_FAKE_ISCHROOT_STATUS:-1}"
`);
  writeExecutable(path.join(binDir, "deb-systemd-helper"), `#!/bin/sh
printf '%s\n' "deb-systemd-helper $*" >> "\${KILN_VM_DEB_CALL_LOG}"
case "$*" in
  "debian-installed kiln-vmd.service") exit "\${KILN_VM_FAKE_INSTALLED_STATUS:-1}" ;;
  "--quiet was-enabled kiln-vmd.service") exit "\${KILN_VM_FAKE_WAS_ENABLED_STATUS:-1}" ;;
esac
exit "\${KILN_VM_FAKE_HELPER_STATUS:-0}"
`);
  writeExecutable(path.join(binDir, "systemctl"), `#!/bin/sh
printf '%s\n' "systemctl $*" >> "\${KILN_VM_DEB_CALL_LOG}"
exit "\${KILN_VM_FAKE_SYSTEMCTL_STATUS:-0}"
`);
  writeExecutable(path.join(binDir, "deb-systemd-invoke"), `#!/bin/sh
printf '%s\n' "deb-systemd-invoke $*" >> "\${KILN_VM_DEB_CALL_LOG}"
exit "\${KILN_VM_FAKE_INVOKE_STATUS:-0}"
`);
  return { binDir, callLog };
}

function serviceEnvironment(binDir, callLog, runDir) {
  return {
    PATH: `${binDir}:${process.env.PATH}`,
    KILN_VM_DEB_CALL_LOG: callLog,
    KILN_VM_DEB_SYSTEMD_RUN_DIR: runDir,
  };
}

function writeExecutable(target, content) {
  fs.writeFileSync(target, content, { mode: 0o755 });
  fs.chmodSync(target, 0o755);
}

function readOptionalFile(target) {
  return fs.existsSync(target) ? fs.readFileSync(target, "utf8") : "";
}
