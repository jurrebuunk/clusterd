# Builds one of the two programs; both share the source and Go modules.
{ lib, stdenv, buildGoModule, installShellFiles, self }:
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
      ../api # the contract test checks the server against it
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
  nativeBuildInputs = lib.optional (name == "labctl") installShellFiles;
  postInstall = lib.optionalString (name == "labctl" && stdenv.buildPlatform.canExecute stdenv.hostPlatform) ''
    installShellCompletion --cmd labctl \
      --bash <($out/bin/labctl completion bash) \
      --zsh <($out/bin/labctl completion zsh) \
      --fish <($out/bin/labctl completion fish)
  '';
  meta = {
    description =
      if name == "clusterd" then "Platform API for the homelab" else "Command-line client for the clusterd API";
    homepage = "https://github.com/jurrebuunk/clusterd";
    mainProgram = name;
  };
}
