package main

func resource(name string) object {
	return object{"metadata": object{"name": name, "uid": "uid-" + name, "resourceVersion": "1"}}
}
