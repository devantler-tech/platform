package main

import "testing"

func TestCSIAndFilesystemCanariesHaveSeparateConfirmationsAndJournals(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"] = "canary-csi-v2"
	env["LONGHORN_V2_CONFIRM"] = "verify-longhorn-v2-restore"
	getenv := func(k string) string { return env[k] }
	if authorizeCanary(getenv) == nil {
		t.Fatal("filesystem confirmation authorized CSI data movement")
	}
	env["LONGHORN_V2_CONFIRM"] = "verify-longhorn-v2-csi-restore"
	if err := authorizeCanary(getenv); err != nil {
		t.Fatal(err)
	}
	if authorize(getenv, "prod-worker-1") == nil || authorizeTemporaryPreparation(getenv, "temporary-v1-1") == nil || authorizeHostReboot(getenv, "prod-worker-1") == nil {
		t.Fatal("CSI canary authorized a reset or host reboot")
	}
	fsb, err := canaryJournalName("filesystem")
	if err != nil {
		t.Fatal(err)
	}
	csi, err := canaryJournalName("csi")
	if err != nil || csi == fsb {
		t.Fatal("one canary could overwrite the other transport's evidence")
	}
	if _, err = canaryJournalName("arbitrary-journal"); err == nil {
		t.Fatal("transport selected an undeclared journal")
	}
}

func TestCSIRestoreKeepsTheDownloadedPVAndIndependentBackend(t *testing.T) {
	claim, pv, volume, op, source, class := csiScratchFixture("DataDownload")
	proof, err := verifyCSIScratchBinding(claim, pv, volume, op, source, class)
	if err != nil {
		t.Fatal(err)
	}
	claim["metadata"].(object)["uid"] = "restored-claim-uid"
	claim["metadata"].(object)["namespace"] = "longhorn-canary-v2-1234"
	if err = verifyCSIRestoredBinding(proof, claim, pv, volume); err != nil {
		t.Fatal(err)
	}
	volume["metadata"].(object)["uid"] = "replacement-backend"
	if verifyCSIRestoredBinding(proof, claim, pv, volume) == nil {
		t.Fatal("a different backend substituted for the downloaded data")
	}
}
