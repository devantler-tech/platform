package main

import "testing"

func desiredConfigFixture() (object, object) {
	volume := func(name string, minSize, maxSize float64, negative bool) object {
		return object{"metadata": object{"id": name}, "spec": object{
			"type":         "partition",
			"provisioning": object{"diskSelector": object{"match": "system_disk"}, "partitionSpec": object{"minSize": minSize, "maxSize": maxSize, "negativeMaxSize": negative, "grow": false}},
			"encryption":   object{"provider": "luks2", "keys": []any{object{"slot": float64(0), "type": "nodeID", "lockToState": true}}},
		}}
	}
	return volume("r-longhorn", 96*1024*1024*1024, 96*1024*1024*1024, false), volume("EPHEMERAL", 40*1024*1024*1024, 104*1024*1024*1024, true)
}

func TestWipeRequiresTheDesiredLayoutAlreadyLoadedByTalos(t *testing.T) {
	r, e := desiredConfigFixture()
	if err := verifyDesiredLayout(r, e); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(object, object){
		"missing raw configuration": func(r, _ object) { delete(r, "spec") },
		"root still grows across the disk": func(_, e object) {
			e["spec"].(object)["provisioning"].(object)["partitionSpec"].(object)["grow"] = true
		},
		"root reservation is missing": func(_, e object) {
			delete(e["spec"].(object)["provisioning"].(object)["partitionSpec"].(object), "negativeMaxSize")
		},
		"external disk could be selected": func(r, _ object) {
			r["spec"].(object)["provisioning"].(object)["diskSelector"].(object)["match"] = "!system_disk"
		},
		"raw key is not state-locked": func(r, _ object) {
			r["spec"].(object)["encryption"].(object)["keys"].([]any)[0].(object)["lockToState"] = false
		},
		"root partition is too small": func(_, e object) {
			e["spec"].(object)["provisioning"].(object)["partitionSpec"].(object)["minSize"] = float64(1024)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, e := desiredConfigFixture()
			mutate(r, e)
			if err := verifyDesiredLayout(r, e); err == nil {
				t.Fatal("node reset accepted before desired layout was installed")
			}
		})
	}
}
