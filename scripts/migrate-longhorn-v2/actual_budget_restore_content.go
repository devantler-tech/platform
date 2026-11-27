package main

// This is the running Actual Budget application image used to write the
// archived database. Run only its bundled SQLite client and Node filesystem
// APIs. The account database and any WAL are copied to isolated scratch so
// SQLite can replay the archive without writing to the recovered volume.
const actualBudgetProofImage = "docker.io/actualbudget/actual-server:26.10.0@sha256:24645e971da6bb1a1f953d6859e307a0b29be1e8b4130a35b3220c209b5b860c"

const actualBudgetProofCode = `
'use strict';
const fs = require('node:fs');
const zlib = require('node:zlib');
const Database = require('better-sqlite3');
const maxBytes = 64 * 1024 * 1024;

function requireFile(path, allowEmpty = false) {
  const entry = fs.lstatSync(path, { throwIfNoEntry: false });
  if (!entry || !entry.isFile() || entry.size > maxBytes || (!allowEmpty && entry.size === 0)) throw Error();
  return entry;
}

function checkSQLite(path, dir, check) {
  const copy = dir + '/check.sqlite';
  requireFile(path);
  fs.copyFileSync(path, copy, fs.constants.COPYFILE_EXCL);
  if (fs.lstatSync(path + '-wal', { throwIfNoEntry: false })) {
    requireFile(path + '-wal', true);
    fs.copyFileSync(path + '-wal', copy + '-wal', fs.constants.COPYFILE_EXCL);
  }
  try {
    const db = new Database(copy, { fileMustExist: true });
    try {
      const result = db.pragma('quick_check');
      if (result.length !== 1 || result[0].quick_check !== 'ok') throw Error();
      return check(db);
    } finally {
      db.close();
    }
  } finally {
    for (const suffix of ['', '-wal', '-shm']) fs.rmSync(copy + suffix, { force: true });
  }
}

// These budgets use normal single-disk ZIP archives. Bound the directory,
// member count, input bytes and expanded bytes independently. Only extract
// the selected pair into memory, never an archive-supplied file path.
function readBudgetZip(buffer) {
  let end = buffer.length - 22;
  const first = Math.max(0, end - 65535);
  while (end >= first &&
         (buffer.readUInt32LE(end) !== 0x06054b50 || end + 22 + buffer.readUInt16LE(end + 20) !== buffer.length)) end--;
  if (end < first || buffer.readUInt32LE(end + 4) !== 0) throw Error();
  const count = buffer.readUInt16LE(end + 10);
  const start = buffer.readUInt32LE(end + 16);
  if (count < 2 || count > 128 || count !== buffer.readUInt16LE(end + 8) ||
      start + buffer.readUInt32LE(end + 12) !== end) throw Error();
  const members = new Map();
  let next = start;
  for (let i = 0; i < count; i++) {
    if (next + 46 > end || buffer.readUInt32LE(next) !== 0x02014b50) throw Error();
    const nameLength = buffer.readUInt16LE(next + 28);
    const last = next + 46 + nameLength + buffer.readUInt16LE(next + 30) + buffer.readUInt16LE(next + 32);
    if (nameLength === 0 || last > end) throw Error();
    const name = buffer.toString('utf8', next + 46, next + 46 + nameLength);
    if (members.has(name)) throw Error();
    members.set(name, next);
    next = last;
  }
  if (next !== end) throw Error();
  const suffix = 'db.sqlite';
  const prefixes = [...members.keys()]
    .filter(name => name === suffix || name.endsWith('/' + suffix))
    .map(name => name.slice(0, -suffix.length))
    .filter(prefix => members.has(prefix + 'metadata.json'));
  const prefix = prefixes.includes('') ? '' : prefixes.length === 1 ? prefixes[0] : null;
  if (prefix === null) throw Error();
  const extract = (name, limit) => {
    const dir = members.get(prefix + name);
    const method = buffer.readUInt16LE(dir + 10);
    const size = buffer.readUInt32LE(dir + 24);
    const compressed = buffer.readUInt32LE(dir + 20);
    const local = buffer.readUInt32LE(dir + 42);
    if (size === 0 || size > limit || local + 30 > start || buffer.readUInt32LE(local) !== 0x04034b50 ||
        (buffer.readUInt16LE(dir + 8) & 1) !== 0 || buffer.readUInt16LE(local + 8) !== method) throw Error();
    const nameEnd = local + 30 + buffer.readUInt16LE(local + 26);
    const data = nameEnd + buffer.readUInt16LE(local + 28);
    if (data + compressed > start || buffer.toString('utf8', local + 30, nameEnd) !== prefix + name) throw Error();
    const input = buffer.subarray(data, data + compressed);
    const result = method === 0 ? input : method === 8 ? zlib.inflateRawSync(input, { maxOutputLength: limit }) : null;
    if (!result || result.length !== size || zlib.crc32(result) !== buffer.readUInt32LE(dir + 16)) throw Error();
    return result;
  };
  const metadata = JSON.parse(extract('metadata.json', 1024 * 1024).toString('utf8'));
  if (!metadata || typeof metadata !== 'object' || Array.isArray(metadata)) throw Error();
  return extract('db.sqlite', maxBytes);
}

try {
  for (const directory of ['/data/server-files', '/data/user-files']) {
    if (!fs.lstatSync(directory).isDirectory()) throw Error();
  }
  const dir = fs.mkdtempSync('/tmp/actual-budget-check-');
  const files = checkSQLite('/data/server-files/account.sqlite', dir, db =>
    db.prepare('SELECT id, group_id, encrypt_meta, encrypt_keyid FROM files WHERE deleted = 0 LIMIT 10001').all());
  if (files.length === 0 || files.length > 10000) throw Error();
  for (const { id, group_id: group, encrypt_meta: encryption, encrypt_keyid: key } of files) {
    if (typeof id !== 'string' || !/^[a-zA-Z0-9_-]+$/.test(id)) throw Error();
    if (group !== null) {
      if (typeof group !== 'string' || !/^[a-zA-Z0-9_-]+$/.test(group)) throw Error();
      const messages = '/data/user-files/group-' + group + '.sqlite';
      if (fs.lstatSync(messages, { throwIfNoEntry: false })) {
        checkSQLite(messages, dir, db => {
          db.prepare('SELECT timestamp, is_encrypted, content FROM messages_binary LIMIT 1').get();
          db.prepare('SELECT merkle FROM messages_merkles LIMIT 1').get();
        });
      } else if (fs.lstatSync(messages + '-wal', { throwIfNoEntry: false }) ||
                 fs.lstatSync(messages + '-shm', { throwIfNoEntry: false })) throw Error();
      // Actual creates the group database on first synchronization.
    }
    if (encryption !== null || key !== null) {
      process.stderr.write('ACTUAL_BUDGET_RESTORED_CONTENT=CLIENT_DECRYPTION_REQUIRED\n');
      process.exit(2);
    }
    const blob = '/data/user-files/file-' + id + '.blob';
    requireFile(blob);
    const budget = dir + '/budget.sqlite';
    fs.writeFileSync(budget, readBudgetZip(fs.readFileSync(blob)), { flag: 'wx' });
    checkSQLite(budget, dir, db => {
      db.prepare('SELECT id FROM transactions LIMIT 1').get();
    });
    fs.unlinkSync(budget);
  }
  process.stdout.write('ACTUAL_BUDGET_RESTORED_CONTENT=VERIFIED\n');
  setTimeout(() => {}, 240000);
} catch (_) {
  process.stderr.write('ACTUAL_BUDGET_RESTORED_CONTENT=UNVERIFIED\n');
  process.exitCode = 1;
}
`

func actualBudgetProofPod(namespace, run string) object {
	return object{"apiVersion": "v1", "kind": "Pod", "metadata": object{"namespace": namespace, "name": "verify-restored-budget",
		"labels": object{"storage.platform.devantler.tech/recovery-run": run, "velero.io/exclude-from-backup": "true"}},
		"spec": object{
			"restartPolicy": "Never", "activeDeadlineSeconds": float64(300), "automountServiceAccountToken": false, "hostUsers": false,
			"securityContext": object{
				"runAsNonRoot": true, "runAsUser": float64(1001), "runAsGroup": float64(1001), "fsGroup": float64(1001),
				"fsGroupChangePolicy": "OnRootMismatch", "seccompProfile": object{"type": "RuntimeDefault"},
			},
			"containers": []any{object{
				"name": "verify", "image": actualBudgetProofImage, "command": []any{"node", "-e", actualBudgetProofCode},
				"workingDir": "/app", "env": []any{object{"name": "NODE_PATH", "value": "/app/node_modules"}},
				"securityContext": object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
					"capabilities": object{"drop": []any{"ALL"}}, "runAsNonRoot": true,
					"seccompProfile": object{"type": "RuntimeDefault"}, "seLinuxOptions": object{}},
				"resources": object{"requests": object{"cpu": "100m", "memory": "128Mi"}, "limits": object{"memory": "512Mi"}},
				"volumeMounts": []any{object{"name": "restored-data", "mountPath": "/data", "readOnly": true},
					object{"name": "check-tmp", "mountPath": "/tmp"}},
			}},
			"volumes": []any{object{"name": "restored-data", "persistentVolumeClaim": object{"claimName": actualBudgetClaim, "readOnly": true}},
				object{"name": "check-tmp", "emptyDir": object{"sizeLimit": "160Mi"}}},
		}}
}
