# Brand assets

Everything in `web/public/` is generated from one master image and copied verbatim
into the built console by Vite, then embedded into the binary by
`internal/gateway/ui`.

| File (in `web/public/`) | Used by |
|---|---|
| `favicon.ico` | the browser tab — multi-resolution (16/32/48/64) |
| `favicon-32.png` | browsers that prefer a PNG icon |
| `apple-touch-icon.png` | iOS home screen, 180×180 |
| `logo-64.png` | the console's sidebar mark, drawn at 26px |
| `logo-512.png` | anything that wants a large one |

## Regenerating

The master is `layer-lobster-claw-logo.png` (277×269) in the workspace
root, *outside* this repo. Keep it there and regenerate rather than
editing anything in this directory by hand.

```python
from PIL import Image

src = Image.open("layer-lobster-claw-logo.png").convert("RGBA")

# Square it by padding, never by stretching: the artwork is 277x269
# and squashing it to a square distorts the claw. The transparent
# margin costs nothing.
side = max(src.size)
square = Image.new("RGBA", (side, side), (0, 0, 0, 0))
square.paste(src, ((side - src.width) // 2, (side - src.height) // 2))

for size, name in [(512, "logo-512.png"), (180, "apple-touch-icon.png"),
                   (64, "logo-64.png"), (32, "favicon-32.png")]:
    square.resize((size, size), Image.LANCZOS).save(name, optimize=True)

square.resize((256, 256), Image.LANCZOS).save(
    "favicon.ico", sizes=[(16, 16), (32, 32), (48, 48), (64, 64)])
```

## Two things worth knowing

**The `.ico` is multi-resolution on purpose.** A single large PNG
declared as the icon leaves the browser to downscale it for a 16px tab
strip, and browsers are worse at that than Lanczos is. The `.ico`
carries its own 16 and 32 renderings so the tab gets a real one.

**The mark is a detailed illustration and has a floor.** It reads well
at 26px and up; below roughly 24px the claw and the hex nodes merge.
That is why the sidebar draws it at 26 rather than matching the 8px
dot it replaced. If you ever need something genuinely tiny, it wants a
simplified mark rather than a smaller copy of this one.

## Why this file is not in `web/public/`

Anything in `public/` is copied verbatim into the build and embedded
into the binary, so a README left there would be served at
`/README.md` and would ship inside every release. Notes about the
assets belong next to them in the source tree, not inside the
artefact.
