{ self, ... }:
{
  perSystem =
    {
      lib,
      pkgs,
      self',
      ...
    }:
    let
      version = self.shortRev or self.dirtyShortRev or "dev";
    in
    {
      packages = {
        notmuch-mcp = pkgs.buildGo127Module {
          pname = "notmuch-mcp";
          inherit version;
          src = lib.fileset.toSource {
            root = ../.;
            fileset = lib.fileset.difference (lib.fileset.gitTracked ../.) (
              lib.fileset.unions [
                ../flake.nix
                ../flake.lock
                ../nix
              ]
            );
          };

          # go-sum: 57446ebd86d46450ecfeefab6081509311f6ac964ee6bc8e03f4bea19ea32f61
          vendorHash = "sha256-UhWfSpKha7IenpqTjfXqMpgtA/sf2KcBFppA/7SHVfY=";

          ldflags = [
            "-s"
            "-w"
            "-X main.Version=${version}"
            "-X main.Commit=${self.shortRev or self.dirtyShortRev or "dirty"}"
            "-X main.BuildDate=1970-01-01T00:00:00Z"
          ];

          # The server shells out to the system `notmuch` binary at runtime —
          # propagate it so `nix run` users get a working install.
          propagatedBuildInputs = [ pkgs.notmuch ];

          meta = {
            description = "MCP server for reading and tagging mail with notmuch";
            homepage = "https://github.com/stubbedev/notmuch-mcp";
            mainProgram = "notmuch-mcp";
          };
        };

        default = self'.packages.notmuch-mcp;
      };
    };
}
