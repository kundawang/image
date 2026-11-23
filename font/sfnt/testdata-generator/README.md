# COLR/CPAL test fixture generator

This directory contains a standalone program (build tag `//go:build ignore`)
that builds the deterministic SFNT font fixtures used by the COLR/CPAL tests
in the parent package:

- `colrv0.ttf` — COLR version 0 with a CPAL version 1 table (two palettes),
  foreground and missing-color fallback layers, a semi-transparent layer, a
  fully transparent layer and an empty base glyph.
- `colrv1.ttf` — COLR version 1 exercising PaintSolid, PaintVarSolid,
  PaintGlyph, PaintColrLayers, PaintColrGlyph, an empty layer list, a
  self-referential cycle, an unsupported paint format and an out-of-range
  glyph id.
- `colrv1-deep.ttf` — an acyclic PaintGlyph chain deeper than the parser's
  `maxColorPaintDepth` limit.
- `colr.ttc` — a TrueType Collection containing the two main fonts.

The fonts are assembled byte by byte rather than produced by a font editor so
that their table layout, glyph IDs, palette indices and every offset are
explicit and reproducible.

Regenerate after changing a fixture with:

```
go run gen.go gen_v1.go ../../testdata
```

from this directory. The output is deterministic: running the generator twice
produces byte-identical files.
