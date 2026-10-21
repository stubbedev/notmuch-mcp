{
  perSystem =
    { pkgs, self', ... }:
    {
      devShells.default = pkgs.mkShell {
        inputsFrom = [ self'.packages.default ];

        buildInputs = with pkgs; [
          gopls
          golangci-lint
          just
        ];
      };

      formatter = pkgs.nixfmt;
    };
}
