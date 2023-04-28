{
  description = "Git-fat packaged using poetry2nix";

  inputs.flake-utils.url = "github:numtide/flake-utils";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  inputs.poetry2nix = {
    url = "github:nix-community/poetry2nix";
    inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs = {
    self,
    nixpkgs,
    flake-utils,
    poetry2nix,
  }:
    flake-utils.lib.eachDefaultSystem (system: let
      # see https://github.com/nix-community/poetry2nix/tree/master#api for more functions and examples.
      inherit (poetry2nix.legacyPackages.${system}) mkPoetryApplication mkPoetryEnv;
      poetryEnv = mkPoetryEnv {
        projectDir = ./.;
        overrides = [
          pkgs.poetry2nix.defaultPoetryOverrides
        ];
        editablePackageSources = {
          git-fat = ./.;
        };
      };
      pkgs = nixpkgs.legacyPackages.${system};
    in {
      packages = {
        git-fat = mkPoetryApplication {projectDir = self;};
        default = self.packages.${system}.git-fat;
      };

      devShells.default = pkgs.mkShell {
        buildInputs = with pkgs; [
          python3
          poetry
          poetryEnv
        ];
      };
    });
}
