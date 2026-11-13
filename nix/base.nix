{
  pkgs,
  src ? ../.,
}:

{
  inherit src;

  pname = "gluppencode";
  modules = ./gomod2nix.toml;

  buildInputs =
    pkgs: with pkgs; [
      libadwaita
      gtksourceview5
      pcre2
    ];

  files = {
    desktop = {
      name = "so.libdb.gluppencode.desktop";
      path = ./so.libdb.gluppencode.desktop;
    };
    service = {
      name = "so.libdb.gluppencode.service";
      path = ./so.libdb.gluppencode.service;
    };
    icons = {
      path = pkgs.linkFarm "gluppencode-icons" (
        map
          (path: {
            name = baseNameOf path;
            inherit path;
          })
          [
            ../internal/icons/hicolor
            ../internal/icons/symbolic
          ]
      );
    };
  };
}
