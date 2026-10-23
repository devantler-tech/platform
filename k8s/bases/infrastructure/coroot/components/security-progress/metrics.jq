# Data reduction only: input snapshots stay inside the collector's emptyDir.
# No scan, exception, security gate or issue is changed by this exporter.
def metric($name; $labels; $value):
  if ($value | type) != "number" or $value < 0 then error("invalid metric value") else
  $name + (if ($labels | length) == 0 then "" else
    "{" + ($labels | to_entries | map(.key + "=" + (.value | tostring | tojson)) | join(",")) + "}" end)
  + " " + ($value | tostring) end;
def list: if (.items | type) == "array" then .items else error("missing List") end;
def digest: capture("(?<digest>sha256:[0-9a-f]{64})$").digest;
def scope_key: [.kind, .namespace, .name] | join("/");

if .mode == "policy" then
  ([.policy, .cluster_policy] | map(list) | add
    | [ .[] as $report | $report.results[]?
      | if (.source | type) != "string" then error("missing policy source") else . end
      | select(.source == "kyverno" or (.source | startswith("Kyverno")))
      | if (.source | IN("kyverno","KyvernoGeneratingPolicy","KyvernoMutatingPolicy","KyvernoValidatingPolicy") | not)
        then error("unsupported Kyverno policy source") else . end | . as $r
      | ($r.rule // (if $r.source != "kyverno" then "(policy)" else null end)) as $rule
      | (if ($r.resources | length) > 0 then $r.resources[] else $report.scope end) as $resource
      | if ($resource.kind | type) != "string" or ($resource.name | type) != "string" or
           ($r.policy | type) != "string" or ($rule | type) != "string" or
           ([$r.result] | inside(["pass","fail","warn","error","skip"]) | not)
        then error("unrecognized policy result") else
        {source:$r.source,policy:$r.policy,rule:$rule,namespace:($resource.namespace // $report.metadata.namespace // "_"),
         kind:$resource.kind,name:$resource.name,status:$r.result,severity:($r.severity // $r.properties.severity // "unknown" | ascii_downcase),
         at:($r.timestamp.seconds // 0)} end ]
    | group_by([.source,.policy,.rule,.namespace,.kind,.name])
    | map((map(.at) | max) as $at | map(select(.at == $at)) | unique
      | if (map(.status) | unique | length) > 1 then error("conflicting policy reports") else .[0] end)
  ) as $results
  | if ($results | length) == 0 then error("no policy evidence") else
    (["pass","fail","warn","error","skip"][] as $status
      | metric("platform_security_policy_total"; {status:$status}; ([$results[] | select(.status == $status)] | length))),
    ($results | group_by([.source,.policy,.rule,.namespace,.status,.severity])[]
      | .[0] as $r | metric("platform_security_policy_results"; ($r | {source,policy,rule,namespace,status,severity}); length)),
    ($results[] | select(.status != "pass")
      | metric("platform_security_policy_finding"; {source,policy,rule,namespace,kind,name,status,severity}; 1))
  end
elif .mode == "kubescape" then . as $input
  | if ($input.vulnerability | list | length) == 0 or ($input.vulnerability_stored | list | length) == 0
    then error("missing vulnerability input collection") else . end
  | ($input.coverage_report | split("\n") | map(select(startswith("COVERAGE ")))
    | map(capture("^COVERAGE (?<surface>posture|vulnerability|runtime) (?<values>.*)$")
      | .surface as $surface | .values | split(" ") | map(capture("^(?<state>[a-z]+)=(?<value>[0-9]+)$")
        | {surface:$surface,state,value:(.value|tonumber)})) | add) as $coverage
  | if (["posture","vulnerability","runtime"] - ([$coverage[].surface] | unique) | length) != 0 or
       any($coverage[]; .state == "expected" and .value == 0)
    then error("incomplete coverage report") else
  ($input.workloads | list | map(select(.metadata.namespace as $ns | $input.unscanned | index($ns) == null))
    | map({kind,namespace:.metadata.namespace,name:.metadata.name} | scope_key)) as $expected
  | ([$input.posture | list[] | .metadata.labels as $l
      | {kind:$l["kubescape.io/workload-kind"],namespace:$l["kubescape.io/workload-namespace"],name:$l["kubescape.io/workload-name"]} as $scope
      | select(($scope | scope_key) as $key | $expected | index($key) != null)
      | (.spec.controls // {}) | if type != "object" then error("invalid posture controls") else . end
      | to_entries[] | .key as $control | .value
      | if (.status.status | type) != "string" then error("missing posture status") else
        {control:$control,severity:(.severity.severity // "unknown" | ascii_downcase),
         status:(if .status.status != "failed" and
           ((.status.subStatus | IN("exception","excepted")) or any(.rules[]?; (.appliedIgnoreRules | length) > 0))
           then "accepted" else .status.status end)} + $scope end]
      | unique) as $posture
  | ([$input.pods | list[] | select(.metadata.namespace as $ns | $input.unscanned | index($ns) == null)
      | select(any(.metadata.ownerReferences[]?; .kind == "Job") | not) | . as $pod
      | if .status.phase == "Running" then
          (.status.containerStatuses[]?,
            (.status.initContainerStatuses[]? | . as $status | select(any($pod.spec.initContainers[]?; .name == $status.name and .restartPolicy == "Always"))),
            (.status.ephemeralContainerStatuses[]? | select(.state.running != null)))
        elif .status.phase == "Pending" then
          .status.initContainerStatuses[]? | . as $status | select(.state.running != null and any($pod.spec.initContainers[]?; .name == $status.name and .restartPolicy == "Always"))
        else empty end | .imageID | digest] | unique) as $images
  | ($input.vulnerability | list | map({key:(.metadata.namespace+"/"+.metadata.name),value:.metadata.resourceVersion}) | from_entries) as $listed
  | ([$input.vulnerability_stored | list[] | .metadata.annotations as $a
      | select($listed[.metadata.namespace+"/"+.metadata.name] == .metadata.resourceVersion)
      | ($a["kubescape.io/image-id"] | digest) as $digest
      | select($images | index($digest) != null)
      | ($a["kubescape.io/timestamp"] | tonumber) as $at
      | select($at <= $input.now and $input.now - $at <= 604800)
      | if (.spec.severities | type) != "object" or (.spec.severities | length) == 0 or
           (all(.spec.severities[]; type == "object" and has("all") and
             (.all | type == "number" and . >= 0 and floor == .)) | not) or
           ((.spec.severities | keys) - ["critical","high","medium","low","negligible","unknown"] | length) != 0
        then error("missing vulnerability severities") else
        {image:$digest,image_ref:$a["kubescape.io/image-id"],at:$at,
         severities:(.spec.severities | with_entries(.value = {all:.value.all}))} end]
      | group_by(.image) | map((map(.at) | max) as $at | map(select(.at == $at))
        | if (map(.severities) | unique | length) > 1 then error("conflicting current vulnerability summaries")
          else sort_by(.image_ref) | .[0] end)) as $vulnerabilities
  |
    ($coverage[] | metric("platform_security_coverage"; {surface,state}; .value)),
    metric("platform_security_posture_freshness_observable"; {}; 0),
    metric("platform_security_runtime_delivery_verified"; {}; 0),
    (if ($posture | length) > 0 then
      (["passed","failed","skipped","accepted","unknown"] + [$posture[].status] | unique)[] as $status
      | metric("platform_security_posture_total"; {status:$status}; ([$posture[] | select(.status == $status)] | length))
      else empty end),
    ($posture | group_by([.control,.severity,.status])[] | .[0] as $r
      | metric("platform_security_posture_results"; ($r | {control,severity,status}); length)),
    ($posture[] | select(.status != "passed") | metric("platform_security_posture_finding"; {control,severity,status,namespace,kind,name}; 1)),
    (if ($vulnerabilities | length) > 0 then ["critical","high","medium","low","negligible","unknown"][] as $severity
      | ($vulnerabilities | map(.severities[$severity].all // 0) | add) as $total
      | metric("platform_security_vulnerabilities"; {severity:$severity,context:"full"}; $total) else empty end),
    ($vulnerabilities[] | . as $v | ["critical","high","medium","low","negligible","unknown"][] as $severity
      | metric("platform_security_vulnerability_image"; {image:$v.image_ref,severity:$severity,context:"full"}; ($v.severities[$severity].all // 0)))
  end
else error("unknown collection mode") end
