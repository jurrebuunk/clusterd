# Builds one of the two programs; both share the source and Go modules.
{ lib, buildGoModule, self }:
name:
let
  version = lib.fileContents ../VERSION;
in
buildGoModule {
  pname = name;
  inherit version;
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
    ];
  };
  vendorHash = "sha256-F6WucqRB14LsXuPOPYlbRfS0Voex3YJVoaWw3X3Mgbk=";
  subPackages = [ "cmd/${name}" ];
  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X github.com/jurrebuunk/clusterd/internal/version.Version=${version}"
    "-X github.com/jurrebuunk/clusterd/internal/version.Commit=${self.shortRev or self.dirtyShortRev or "unknown"}"
    "-X github.com/jurrebuunk/clusterd/internal/version.Date=${self.lastModifiedDate or "unknown"}"
  ];
  meta = {
    description =
      if name == "clusterd" then "Platform API for the homelab" else "Command-line client for the clusterd API";
    homepage = "https://github.com/jurrebuunk/clusterd";
    mainProgram = name;
  };
}
