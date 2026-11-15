# Devantler Tech brand

The logo and colours the platform's self-hosted pages use. The maintainer approved this logo on
2026-11-15 and asked for the same green style on light and dark backgrounds, so there is no
inverted version.

## Logo

| File | Use it for |
| --- | --- |
| [`devantler-tech-logo.svg`](images/brand/devantler-tech-logo.svg) | Anything square: favicons, app and touch icons, avatars. A near-black tile (`#0b0f0c`), a thin `#39ff14` ring and the green orb with "DT". |
| [`devantler-tech-logo-transparent.svg`](images/brand/devantler-tech-logo-transparent.svg) | Page headers and sign-in screens, on light and dark backgrounds alike. The same mark without the tile, with a `#2fc413` ring. |

These two SVGs are the vector source. "DT" is set in Monaspace Krypton Bold (SIL Open Font
License) and converted to outlines, so no font is needed to draw it. Export PNGs from the SVGs at
the size a page needs, rather than scaling another PNG.

KSail publishes only YAML and JSON files, so a page in the cluster cannot load these files from
here. Each one embeds the files it needs in its own ConfigMap:

- Dex: [`dex-theme`](../k8s/bases/infrastructure/controllers/dex/config-map.yaml) holds
  `favicon.png` (the square logo at 256 px) and `logo.svg` (the transparent logo).
- Homepage: [`homepage-brand`](../k8s/bases/apps/homepage/config-map-brand.yaml) holds the square
  logo at 180, 192 and 512 px and the transparent `logo.svg`.

devantler.tech uses the same square logo at 256 px as its `favicon.png`
(devantler-tech/business-site `public/favicon.png`). When the logo changes, update every copy above
in the same change.

## Colours

| Name | Value |
| --- | --- |
| Accent (neon green) | `#39ff14` |
| Accent, lighter | `#7aff6e` |
| Accent, dark tint | `#0a2e14` |
| Accent on light backgrounds | `#15803d` |
| Logo tile | `#0b0f0c` |
| Page background on the platform's dark pages | `hsl(224, 10%, 10%)` |

The shared brand kit with icon sets and colour tokens is tracked in
[#5054](https://github.com/devantler-tech/platform/issues/5054).
