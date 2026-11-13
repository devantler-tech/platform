package backupcatalogue

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
)

const s3Namespace = "http://s3.amazonaws.com/doc/2006-03-01/"

// Unique fields and a singular complete document are required before decoding
// into ordinary Go structs, whose decoders otherwise accept duplicate fields.
func singularXML(data []byte, root string, repeatedContents bool) bool {
	d := xml.NewDecoder(bytes.NewReader(data))
	var frames []map[string]bool
	roots, nodes := 0, 0
	space := ""
	for {
		token, err := d.Token()
		if err == io.EOF {
			return roots == 1 && len(frames) == 0
		}
		if err != nil {
			return false
		}
		switch token := token.(type) {
		case xml.StartElement:
			nodes++
			if nodes > 16384 || len(frames) >= 8 {
				return false
			}
			if len(frames) == 0 {
				roots++
				space = token.Name.Space
				if roots != 1 || token.Name.Local != root || space != "" && space != s3Namespace {
					return false
				}
			} else {
				if token.Name.Space != space {
					return false
				}
				parent := frames[len(frames)-1]
				repeatAllowed := repeatedContents && len(frames) == 1 && token.Name.Local == "Contents"
				if parent[token.Name.Local] && !repeatAllowed {
					return false
				}
				parent[token.Name.Local] = true
			}
			frames = append(frames, map[string]bool{})
		case xml.EndElement:
			if len(frames) == 0 {
				return false
			}
			frames = frames[:len(frames)-1]
		case xml.Directive:
			return false
		case xml.ProcInst:
			if roots != 0 || token.Target != "xml" {
				return false
			}
		case xml.CharData:
			if len(frames) == 0 && strings.TrimSpace(string(token)) != "" {
				return false
			}
		}
	}
}
