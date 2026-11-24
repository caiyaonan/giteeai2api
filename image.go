package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"

	_ "golang.org/x/image/webp"
)

// detectFormat 返回 png / jpeg / webp
func detectFormat(b []byte) string {
	switch http.DetectContentType(b) {
	case "image/jpeg":
		return "jpeg"
	case "image/webp":
		return "webp"
	case "image/png":
		return "png"
	}
	if len(b) > 12 && string(b[8:12]) == "WEBP" {
		return "webp"
	}
	return "png"
}

// convertImage 按 output_format 转码；格式相同则零拷贝直接返回。
// webp 编码标准库不支持，此时保留原图并返回实际格式。
func convertImage(b []byte, want string, compression *int) ([]byte, string) {
	src := detectFormat(b)
	if want == "jpg" {
		want = "jpeg"
	}
	if want == "" || want == src || want == "webp" {
		return b, src
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return b, src
	}
	var buf bytes.Buffer
	switch want {
	case "jpeg":
		q := 90
		if compression != nil && *compression > 0 && *compression <= 100 {
			q = *compression
		}
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: q})
	case "png":
		err = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, img)
	default:
		return b, src
	}
	if err != nil {
		return b, src
	}
	return buf.Bytes(), want
}

func mimeOf(format string) string {
	switch format {
	case "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	}
	return "image/png"
}

func extOf(format string) string {
	if format == "jpeg" {
		return "jpg"
	}
	return format
}
