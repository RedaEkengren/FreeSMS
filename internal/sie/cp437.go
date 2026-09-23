// Package sie writes the Swedish accounting interchange format.
package sie

import "strings"

// cp437 maps the Unicode characters a Swedish workshop actually types to their
// code page 437 bytes.
//
// The SIE specification says PC8, which is code page 437 -- not UTF-8, and not
// Latin-1 either. A file written as UTF-8 imports with every Swedish character
// mangled, which reads as the shop's data being corrupt rather than the file
// being wrong. It is the single most common way this export goes out broken.
var cp437 = map[rune]byte{
	'Ç': 128, 'ü': 129, 'é': 130, 'â': 131, 'ä': 132, 'à': 133, 'å': 134, 'ç': 135,
	'ê': 136, 'ë': 137, 'è': 138, 'ï': 139, 'î': 140, 'ì': 141, 'Ä': 142, 'Å': 143,
	'É': 144, 'æ': 145, 'Æ': 146, 'ô': 147, 'ö': 148, 'ò': 149, 'û': 150, 'ù': 151,
	'ÿ': 152, 'Ö': 153, 'Ü': 154, '¢': 155, '£': 156, '¥': 157, 'ƒ': 159,
	'á': 160, 'í': 161, 'ó': 162, 'ú': 163, 'ñ': 164, 'Ñ': 165,
	'¿': 168, '½': 171, '¼': 172, '¡': 173, '«': 174, '»': 175,
	'ß': 225, 'µ': 230, '±': 241, '÷': 246, '°': 248, '·': 250, '²': 253,
}

// encode turns a Go string into CP437 bytes.
//
// Anything with no place in the code page becomes a question mark rather than
// silently disappearing or producing a byte that means something else. A name
// that arrives as Ob?rg is obviously wrong; one that arrives as Ob„rg looks
// like the database is damaged.
func encode(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r < 128:
			out = append(out, byte(r))
		default:
			if b, ok := cp437[r]; ok {
				out = append(out, b)
			} else {
				out = append(out, '?')
			}
		}
	}
	return out
}

// quote wraps a field in the quotes SIE uses, escaping what would end it.
func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
