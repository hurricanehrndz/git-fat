{
  description = "git-fat: store large files outside git";

  inputs.flake-utils.url = "github:numtide/flake-utils";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  # Dev shell only. There is no packages.default: buildGoModule builds with
  # GOTOOLCHAIN=local, and go.mod needs a newer Go than nixpkgs ships (go.mod
  # inherits 1.26.9 from the pinsync test dependency; nixpkgs has 1.26.8).
  # Add packages.default back (buildGoModule) once nixpkgs' Go is at least
  # the go.mod version.
  outputs = {
    nixpkgs,
    flake-utils,
    ...
  }:
    flake-utils.lib.eachDefaultSystem (system: let
      pkgs = nixpkgs.legacyPackages.${system};
    in {
      # GOTOOLCHAIN keeps its default (auto), so go downloads the go.mod
      # version on first use.
      devShells.default = pkgs.mkShell {
        packages = with pkgs; [
          go
          golangci-lint
          gofumpt
          just
          rustfs # spawned by `go test -tags integration`
        ];
      };
    });
}
