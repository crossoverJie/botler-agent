package wechat

import (
	"encoding/base64"
	"fmt"
	"strings"

	"rsc.io/qr"
)

// renderPairQR encodes the QR content for the WebUI as a PNG data URL (v1 used
// qrcode.toDataURL). rsc.io/qr is the one added dependency: encoding a QR code by hand is not
// something to reimplement, and the same package also backs the CLI's terminal QR.
func renderPairQR(content string) (string, error) {
	code, err := qr.Encode(content, qr.M)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG()), nil
}

// RenderPairQR is the exported form used by the WebUI pairing endpoint.
func RenderPairQR(content string) (string, error) { return renderPairQR(content) }

// renderASCIIQR renders a QR code for the terminal (v1 used qrcode-terminal with {small: true}).
// Two module rows are packed into one text row using half blocks, with a 2-module quiet zone.
func renderASCIIQR(content string) (string, error) {
	code, err := qr.Encode(content, qr.M)
	if err != nil {
		return "", err
	}
	const quiet = 2
	size := code.Size
	black := func(x, y int) bool {
		if x < 0 || y < 0 || x >= size || y >= size {
			return false
		}
		return code.Black(x, y)
	}
	var b strings.Builder
	for y := -quiet; y < size+quiet; y += 2 {
		for x := -quiet; x < size+quiet; x++ {
			top := black(x, y)
			bottom := black(x, y+1)
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// printTerminalQR prints the terminal QR, falling back to the raw content when encoding fails.
func printTerminalQR(content string) {
	art, err := renderASCIIQR(content)
	if err != nil {
		fmt.Printf("QR content (open in a browser): %s\n", content)
		return
	}
	fmt.Println(art)
}
