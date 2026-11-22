// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package webp implements a decoder for WEBP images.
//
// WEBP is defined at:
// https://developers.google.com/speed/webp/docs/riff_container
//
// Decode and DecodeConfig only report pixels and image configuration.
// DecodeConfigWithMetadata additionally reports the file's EXIF, XMP, ICC
// profile and unrecognized metadata chunks in a single pass that does not
// decode pixels or require a seekable reader.
package webp // import "golang.org/x/image/webp"
