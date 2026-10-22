# Closed source-publication contract for WebApp instances. API-server pruning
# is not a source-validation boundary. Keep the compiler's real admission and
# resolved-workload controls alongside this independently executable check.
def only($keys): type == "object" and ((keys_unsorted - $keys) | length == 0);
def optional($key; rule): (has($key) | not) or (.[$key] | rule);
def text: type == "string" and length > 0;
def identity: type == "number" and . == floor and . >= 1 and . <= 2147483647;
def argument:
  type == "string" and utf8bytelength <= 256 and
  (contains("\u0000") | not) and (contains("$(") | not);
def item:
  only(["key", "path"]) and has("key") and has("path") and
  (.key | text and length <= 253 and test("^[-._a-zA-Z0-9]+$")) and
  (.path | text and length <= 64 and test("^[A-Za-z0-9][A-Za-z0-9._-]*$"));
def projection:
  only(["secretName", "items"]) and has("secretName") and has("items") and
  (.secretName | text and length <= 253 and
    test("^[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$")) and
  (.items | type == "array" and length >= 1 and length <= 8 and all(.[]; item) and
    ((map(.key) | unique | length) == length) and
    ((map(.path) | unique | length) == length));
def spec:
  only(["name", "image", "host", "port", "replicas", "probePath",
    "cpuRequest", "memoryRequest", "cpuLimit", "memoryLimit", "args",
    "runAsUser", "runAsGroup", "readOnlyRootFilesystem", "secretProjections"]) and
  (.name | text) and (.image | text) and (.host | text) and
  optional("port"; type == "number" and . == floor) and
  optional("replicas"; type == "number" and . == floor) and
  optional("probePath"; type == "string") and
  optional("cpuRequest"; type == "string") and
  optional("memoryRequest"; type == "string") and
  optional("cpuLimit"; type == "string") and
  optional("memoryLimit"; type == "string") and
  optional("args"; type == "array" and length <= 32 and all(.[]; argument)) and
  optional("runAsUser"; identity) and optional("runAsGroup"; identity) and
  optional("readOnlyRootFilesystem"; type == "boolean") and
  optional("secretProjections"; type == "array" and length <= 1 and all(.[]; projection));
length == 1 and (.[0] |
  only(["apiVersion", "kind", "metadata", "spec"]) and
  .apiVersion == "kro.run/v1alpha1" and .kind == "WebApp" and
  (.metadata | type == "object") and (.spec | spec))
