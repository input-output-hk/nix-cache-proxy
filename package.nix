{ buildGoModule, inclusive, rev }:
buildGoModule rec {
  pname = "nix-cache-proxy";
  version = "2022.02.02.001";
  vendorSha256 = "sha256-H5Zolka/dLLi4qkmXM16Nnmf8xQAGbKJ3xbEo+h3Imc=";

  src = inclusive ./. [
    ./fixtures
    ./go.mod
    ./go.sum
    ./helpers.go
    ./main.go
    ./narinfo.go
    ./narinfo_test.go
    ./nix_config.go
    ./router.go
    ./routing_test.go
  ];

  CGO_ENABLED = "0";
  GOOS = "linux";

  ldflags = [
    "-s"
    "-w"
    "-extldflags"
    "-static"
    "-X main.buildVersion=${version} -X main.buildCommit=${rev}"
  ];
}
