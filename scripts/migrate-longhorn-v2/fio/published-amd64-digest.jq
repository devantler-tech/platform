def sha256: type == "string" and test("^sha256:[a-f0-9]{64}$");
def imageManifest:
  . == "application/vnd.oci.image.manifest.v1+json" or
  . == "application/vnd.docker.distribution.manifest.v2+json";

if ([$published, $tested] | all(.[]; sha256) | not) or .schemaVersion != 2 then
  error("invalid published or tested image identity")
elif (.mediaType | imageManifest) then
  if (.config.digest | sha256) and ($tested == $published or $tested == .config.digest) then
    $published
  else
    error("published image manifest differs from the tested image")
  end
elif .mediaType == "application/vnd.oci.image.index.v1+json" or
     .mediaType == "application/vnd.docker.distribution.manifest.list.v2+json" then
  if $tested != $published or (.manifests | type) != "array" then
    error("published image index differs from the tested image")
  else
    [.manifests[] | select(.platform.os == "linux" and .platform.architecture == "amd64")]
    | if length == 1 and (.[0].mediaType | imageManifest) and (.[0].digest | sha256) then
        .[0].digest
      else
        error("expected one Linux amd64 image manifest in the tested index")
      end
  end
else
  error("unsupported published image media type")
end
