{
  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";

    gomod2nix = {
      url = "github:nix-community/gomod2nix";
      inputs.nixpkgs.follows = "nixpkgs";
      inputs.flake-utils.follows = "flake-utils";
    };

    gotk4-nix = {
      url = "github:diamondburned/gotk4-nix";
      inputs.nixpkgs.follows = "nixpkgs";
      inputs.gomod2nix.follows = "gomod2nix";
      inputs.flake-utils.follows = "flake-utils";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      gotk4-nix,
      gomod2nix,
      ...
    }:
    let
      baseFunc = pkgs: {
        src = self;
        pname = "gluppencode";
        buildInputs =
          pkgs: with pkgs; [
            libadwaita
            gtksourceview5
            pcre2
          ];
      };

      goPackage = "go_1_27";
    in
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system}.appendOverlays [
          gomod2nix.overlays.default
          gotk4-nix.overlays.patchelf
        ];

        go = pkgs.${goPackage};
      in
      {
        devShells.default = gotk4-nix.lib.mkShell {
          inherit pkgs go;
          base = baseFunc pkgs;
          buildInputs = with pkgs; [
            jq
            libxml2
            pkgs.gomod2nix
          ];
        };
      }
    );
}
