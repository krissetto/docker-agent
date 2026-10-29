# Custom terminal fonts

Docker Agent's TUI draws terminal cells; the terminal emulator chooses their font.
There is no portable ANSI command to embed or select a font in an existing terminal,
and the Kit guest cannot control the host terminal's font. The Docker Next themes
change colors, not typography.

## Opt-in Ghostty launcher

For a dedicated font without changing your saved terminal settings, run this helper
**on the graphical host**, from the repository checkout:

```sh
./scripts/run-with-font.sh -- ./bin/docker-agent run kit/hackerspace.yaml
```

It opens a separate Ghostty instance using **GT Standard Mono**. Install a licensed
copy on the host first; the helper checks availability and refuses to silently
substitute another font. No font binary or redistribution license was supplied with
this repository, so GT Standard Mono is not bundled or downloaded.

To choose another installed font and an optional size:

```sh
./scripts/run-with-font.sh --font 'Menlo' --font-size 14 -- \
  ./bin/docker-agent run --theme docker-next-dark kit/hackerspace.yaml
```

- Requires Ghostty on macOS or Linux; uses Ghostty 1.3.1's documented command-line
  interface. `--ghostty PATH` selects a custom executable installation.
- Uses a new instance rather than altering your current window or global config.
  On macOS the helper uses `open -n -a Ghostty.app`; Linux invokes Ghostty directly.
- Everything after `--` is forwarded as command arguments without shell evaluation.
  The new terminal starts in the current working directory. Font size, if provided,
  is an integer from 6 to 72 points; otherwise your Ghostty size is retained.
- You can wrap your usual **host-side Kit connection command** in the same way.
  Do not run the helper inside the Kit guest or over SSH. It does not configure
  Docker Desktop's embedded terminal or install anything in a container.
- Font selection applies to the new terminal instance, not to a remote session's
  persisted settings. Environment inheritance follows Ghostty/Launch Services;
  configure credentials through the usual application mechanism, not command text.

For another terminal emulator, select GT Standard Mono in its own font settings.
An app-owned browser or native terminal renderer could load fonts directly, but
that would be a separate frontend rather than a font option for the existing TUI.

## Validation scope

Launcher tests mock macOS/Linux process execution to verify argument preservation,
missing-font rejection, input validation, symlink resolution and SSH rejection.
The font configuration was also accepted by Ghostty 1.3.1's config validator.
No graphical window or real session was launched during these checks; GT Standard
Mono was not installed on the validation host, so its appearance remains untested.
