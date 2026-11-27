package main

// Complete released ObjectStore CRD.spec at Barman 0.16.0 source commit
// cd3b9faeb18466bc1195688589dc20db2b247f15, with Kubernetes' conversion=None
// default. Canonical encoding uses json.Decoder.UseNumber and json.Marshal.
const barmanReviewedCRDSHA = "750db8dc45db1eeb4f190f3b2d4d46c78f96163790e444a151b4c56b979fe10b"

func bindBarmanCRD(raw []byte) (cnpgCRDBinding, error) {
	return bindCNPGRecoveryCRD(raw, "objectstores.barmancloud.cnpg.io", barmanReviewedCRDSHA)
}
