package main

// This is the running Actual Budget application image used to write the
// archived database. Run only its bundled SQLite client and Node filesystem
// APIs. The account database and any WAL are copied to isolated scratch so
// SQLite can replay the archive without writing to the recovered volume.
const actualBudgetProofImage = "docker.io/actualbudget/actual-server:26.10.0@sha256:24645e971da6bb1a1f953d6859e307a0b29be1e8b4130a35b3220c209b5b860c"

const actualBudgetProofCode = `
'use strict';
const fs = require('node:fs');
const Database = require('better-sqlite3');
try {
  const database = 'account.sqlite';
  for (const directory of ['/data/server-files', '/data/user-files']) {
    if (!fs.lstatSync(directory).isDirectory()) throw Error();
  }
  const dir = fs.mkdtempSync('/tmp/actual-budget-check-');
  for (const suffix of ['', '-wal']) {
    const filename = database + suffix;
    const source = '/data/server-files/' + filename;
    const entry = fs.lstatSync(source, { throwIfNoEntry: false });
    if (!entry && suffix !== '') continue;
    if (!entry || !entry.isFile() || entry.size > 64 * 1024 * 1024 || (suffix === '' && entry.size === 0)) throw Error();
    if (entry.size === 0) continue;
    fs.copyFileSync(source, dir + '/' + filename, fs.constants.COPYFILE_EXCL);
  }
  const db = new Database(dir + '/' + database, { fileMustExist: true });
  const check = db.pragma('quick_check');
  if (check.length !== 1 || check[0].quick_check !== 'ok') throw Error();
  const files = db.prepare('SELECT id FROM files WHERE deleted = 0 LIMIT 10001').all();
  if (files.length === 0 || files.length > 10000) throw Error();
  for (const { id } of files) {
    if (typeof id !== 'string' || !/^[a-zA-Z0-9_-]+$/.test(id)) throw Error();
    const blob = fs.lstatSync('/data/user-files/file-' + id + '.blob');
    if (!blob.isFile() || blob.size === 0) throw Error();
  }
  db.close();
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
					"capabilities": object{"drop": []any{"ALL"}}},
				"resources": object{"requests": object{"cpu": "100m", "memory": "64Mi"}, "limits": object{"memory": "128Mi"}},
				"volumeMounts": []any{object{"name": "restored-data", "mountPath": "/data", "readOnly": true},
					object{"name": "check-tmp", "mountPath": "/tmp"}},
			}},
			"volumes": []any{object{"name": "restored-data", "persistentVolumeClaim": object{"claimName": actualBudgetClaim, "readOnly": true}},
				object{"name": "check-tmp", "emptyDir": object{"sizeLimit": "160Mi"}}},
		}}
}
