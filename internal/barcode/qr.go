package barcode

// A QR code (model 2, ISO/IEC 18004), for the payment code on an invoice
// (#234): byte mode, an optional ECI designator, error correction level M,
// versions 1 to 20.
//
// Written out rather than pulled in, as Code 128 was, though it is more than
// a hundred lines: a QR encoder is a fixed algorithm -- a bit stream, Reed-
// Solomon over GF(256), a placement and eight masks -- that does not change,
// so there is nothing to keep up to date, and an invoice page must render
// without asking anybody else. What makes it trustworthy is the same as for
// Code 128: the block table is checked arithmetically against the number of
// modules each version has, and every rendered code is read back by zbar, a
// decoder that shares no code with this, in the CI job that installs it.
// The structure follows Project Nayuki's reference description of the
// algorithm (https://www.nayuki.io/page/qr-code-generator-library).

import (
	"errors"
	"fmt"
	"strings"
)

// qrBlocks is level M's block structure for versions 1 to 20 (ISO/IEC
// 18004:2015, table 9): error correction codewords per block, then the
// number of blocks and data codewords in each of the two groups.
var qrBlocks = [21][5]int{
	{},
	{10, 1, 16, 0, 0}, {16, 1, 28, 0, 0}, {26, 1, 44, 0, 0}, {18, 2, 32, 0, 0}, {24, 2, 43, 0, 0},
	{16, 4, 27, 0, 0}, {18, 4, 31, 0, 0}, {22, 2, 38, 2, 39}, {22, 3, 36, 2, 37}, {26, 4, 43, 1, 44},
	{30, 1, 50, 4, 51}, {22, 6, 36, 2, 37}, {22, 8, 37, 1, 38}, {24, 4, 40, 5, 41}, {24, 5, 41, 5, 42},
	{28, 7, 45, 3, 46}, {28, 10, 46, 1, 47}, {26, 9, 43, 4, 44}, {26, 3, 44, 11, 45}, {26, 3, 41, 13, 42},
}

// qrAlign are the centres of the alignment patterns, per version.
var qrAlign = [21][]int{
	{}, {}, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34}, {6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50},
	{6, 30, 54}, {6, 32, 58}, {6, 34, 62}, {6, 26, 46, 66}, {6, 26, 48, 70}, {6, 26, 50, 74}, {6, 30, 54, 78},
	{6, 30, 56, 82}, {6, 30, 58, 86}, {6, 34, 62, 90},
}

// ErrQRTooLong is data longer than a version 20 code holds.
var ErrQRTooLong = errors.New("barcode: too much for a QR code")

// QR is a QR code's modules, row by row.
type QR struct {
	Size int
	dark []bool
	fn   []bool
}

// Dark reports whether the module in column x, row y is dark.
func (q *QR) Dark(x, y int) bool { return q.dark[y*q.Size+x] }

// qrDataCodewords is how many data codewords a version holds at level M.
func qrDataCodewords(v int) int {
	b := qrBlocks[v]
	return b[1]*b[2] + b[3]*b[4]
}

// qrRawModules is how many modules of a version carry codewords, from the
// geometry alone: the check the block table is held to.
func qrRawModules(v int) int {
	n := (16*v+128)*v + 64
	if v >= 2 {
		align := v/7 + 2
		n -= (25*align-10)*align - 55
		if v >= 7 {
			n -= 36
		}
	}
	return n
}

// EncodeQR makes the smallest code that holds data in byte mode, preceded
// by an ECI designator when eci is above zero -- 26 says the bytes are
// UTF-8, which a reader otherwise may not assume.
func EncodeQR(data []byte, eci int) (*QR, error) {
	if eci < 0 || eci > 127 {
		return nil, fmt.Errorf("barcode: ECI %d is not one this encoder writes", eci)
	}
	for v := 1; v <= 20; v++ {
		count := 8
		if v >= 10 {
			count = 16
		}
		bits := 4 + count + 8*len(data)
		if eci > 0 {
			bits += 12
		}
		if bits > 8*qrDataCodewords(v) || len(data) >= 1<<count {
			continue
		}
		return qrBuild(v, qrStream(v, count, data, eci)), nil
	}
	return nil, ErrQRTooLong
}

// qrStream is the data codewords: mode, count, bytes, terminator, padding.
func qrStream(v, count int, data []byte, eci int) []byte {
	var bits []bool
	put := func(value, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, value>>i&1 == 1)
		}
	}
	if eci > 0 {
		put(0b0111, 4)
		put(eci, 8)
	}
	put(0b0100, 4)
	put(len(data), count)
	for _, b := range data {
		put(int(b), 8)
	}
	capacity := 8 * qrDataCodewords(v)
	put(0, min(4, capacity-len(bits)))
	put(0, (8-len(bits)%8)%8)
	out := make([]byte, 0, capacity/8)
	for i := 0; i < len(bits); i += 8 {
		var b byte
		for j := 0; j < 8; j++ {
			if bits[i+j] {
				b |= 1 << (7 - j)
			}
		}
		out = append(out, b)
	}
	for pad := byte(0xEC); len(out) < capacity/8; pad ^= 0xEC ^ 0x11 {
		out = append(out, pad)
	}
	return out
}

// qrMul multiplies in GF(256) with the QR code's polynomial, 0x11D.
func qrMul(x, y byte) byte {
	z := 0
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x11D)
		z ^= (int(y) >> i & 1) * int(x)
	}
	return byte(z)
}

// qrDivisor is the Reed-Solomon generator of degree n, leading term left out.
func qrDivisor(n int) []byte {
	out := make([]byte, n)
	out[n-1] = 1
	root := byte(1)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			out[j] = qrMul(out[j], root)
			if j+1 < n {
				out[j] ^= out[j+1]
			}
		}
		root = qrMul(root, 2)
	}
	return out
}

// qrRemainder is the error correction codewords of data.
func qrRemainder(data, divisor []byte) []byte {
	out := make([]byte, len(divisor))
	for _, b := range data {
		factor := b ^ out[0]
		copy(out, out[1:])
		out[len(out)-1] = 0
		for i := range out {
			out[i] ^= qrMul(divisor[i], factor)
		}
	}
	return out
}

// qrInterleave splits the data into blocks, adds each block's error
// correction, and interleaves them as they are placed.
func qrInterleave(v int, data []byte) []byte {
	b := qrBlocks[v]
	ec, divisor := b[0], qrDivisor(b[0])
	var blocks [][]byte
	for g, at := 0, 0; g < 2; g++ {
		for i := 0; i < b[1+2*g]; i++ {
			blocks = append(blocks, data[at:at+b[2+2*g]])
			at += b[2+2*g]
		}
	}
	var out []byte
	for i := 0; i < b[4] || i < b[2]; i++ {
		for _, blk := range blocks {
			if i < len(blk) {
				out = append(out, blk[i])
			}
		}
	}
	rems := make([][]byte, len(blocks))
	for i, blk := range blocks {
		rems[i] = qrRemainder(blk, divisor)
	}
	for i := 0; i < ec; i++ {
		for _, r := range rems {
			out = append(out, r[i])
		}
	}
	return out
}

func qrBuild(v int, data []byte) *QR {
	size := 17 + 4*v
	q := &QR{Size: size, dark: make([]bool, size*size), fn: make([]bool, size*size)}
	set := func(x, y int, dark bool) {
		q.dark[y*size+x] = dark
		q.fn[y*size+x] = true
	}
	for i := 0; i < size; i++ {
		set(6, i, i%2 == 0)
		set(i, 6, i%2 == 0)
	}
	for _, c := range [][2]int{{3, 3}, {size - 4, 3}, {3, size - 4}} {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := c[0]+dx, c[1]+dy
				if x < 0 || y < 0 || x >= size || y >= size {
					continue
				}
				d := max(abs(dx), abs(dy))
				set(x, y, d != 2 && d != 4)
			}
		}
	}
	pos := qrAlign[v]
	for i, cx := range pos {
		for j, cy := range pos {
			if (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					set(cx+dx, cy+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	q.format(0, set) // reserved; written for real with the mask chosen
	if v >= 7 {
		rem := v
		for i := 0; i < 12; i++ {
			rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
		}
		bits := v<<12 | rem
		for i := 0; i < 18; i++ {
			a, b := size-11+i%3, i/3
			set(a, b, bits>>i&1 == 1)
			set(b, a, bits>>i&1 == 1)
		}
	}

	all := qrInterleave(v, data)
	i := 0
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = size - 1 - vert
				}
				if !q.fn[y*size+x] && i < len(all)*8 {
					q.dark[y*size+x] = all[i>>3]>>(7-i&7)&1 == 1
					i++
				}
			}
		}
	}

	best, bestPenalty := 0, -1
	for mask := 0; mask < 8; mask++ {
		q.mask(mask)
		q.format(mask, set)
		if p := q.penalty(); bestPenalty < 0 || p < bestPenalty {
			best, bestPenalty = mask, p
		}
		q.mask(mask)
	}
	q.mask(best)
	q.format(best, set)
	return q
}

// format writes the format information for level M and mask, twice.
func (q *QR) format(mask int, set func(x, y int, dark bool)) {
	data := 0<<3 | mask // level M is 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return bits>>i&1 == 1 }
	size := q.Size
	for i := 0; i <= 5; i++ {
		set(8, i, bit(i))
	}
	set(8, 7, bit(6))
	set(8, 8, bit(7))
	set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		set(size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		set(8, size-15+i, bit(i))
	}
	set(8, size-8, true)
}

// mask flips the data modules mask selects; twice undoes it.
func (q *QR) mask(mask int) {
	for y := 0; y < q.Size; y++ {
		for x := 0; x < q.Size; x++ {
			var flip bool
			switch mask {
			case 0:
				flip = (x+y)%2 == 0
			case 1:
				flip = y%2 == 0
			case 2:
				flip = x%3 == 0
			case 3:
				flip = (x+y)%3 == 0
			case 4:
				flip = (x/3+y/2)%2 == 0
			case 5:
				flip = x*y%2+x*y%3 == 0
			case 6:
				flip = (x*y%2+x*y%3)%2 == 0
			case 7:
				flip = ((x+y)%2+x*y%3)%2 == 0
			}
			if flip && !q.fn[y*q.Size+x] {
				q.dark[y*q.Size+x] = !q.dark[y*q.Size+x]
			}
		}
	}
}

// penalty scores a masked code by the standard's four rules; the lowest
// is the mask used. The score only chooses among valid codes.
func (q *QR) penalty() int {
	n := q.Size
	at := func(x, y int) bool { return q.dark[y*n+x] }
	p := 0
	line := func(get func(i int) bool) {
		run := 1
		for i := 1; i <= n; i++ {
			if i < n && get(i) == get(i-1) {
				run++
				continue
			}
			if run >= 5 {
				p += 3 + run - 5
			}
			run = 1
		}
		// 1:1:3:1:1 with four light modules on either side, edges light.
		look := func(i int) bool { return i >= 0 && i < n && get(i) }
		for i := -4; i < n; i++ {
			core := look(i) && !look(i+1) && look(i+2) && look(i+3) && look(i+4) && !look(i+5) && look(i+6)
			if !core {
				continue
			}
			before := !look(i-1) && !look(i-2) && !look(i-3) && !look(i-4)
			after := !look(i+7) && !look(i+8) && !look(i+9) && !look(i+10)
			if before || after {
				p += 40
			}
		}
	}
	for y := 0; y < n; y++ {
		line(func(i int) bool { return at(i, y) })
	}
	for x := 0; x < n; x++ {
		line(func(i int) bool { return at(x, i) })
	}
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if at(x, y) {
				dark++
			}
			if x+1 < n && y+1 < n && at(x, y) == at(x+1, y) && at(x, y) == at(x, y+1) && at(x, y) == at(x+1, y+1) {
				p += 3
			}
		}
	}
	total := n * n
	k := (abs(dark*20-total*10) + total - 1) / total
	return p + max(0, k-1)*10
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// SVG draws the code as one path, a module a unit, with the four-module
// quiet zone the standard requires, scaled by the page. Each row's runs of
// dark modules are one rectangle, which keeps an invoice's code a few
// kilobytes rather than tens.
func (q *QR) SVG(label string) string {
	const quiet = 4
	var d strings.Builder
	for y := 0; y < q.Size; y++ {
		for x := 0; x < q.Size; {
			if !q.Dark(x, y) {
				x++
				continue
			}
			run := 1
			for x+run < q.Size && q.Dark(x+run, y) {
				run++
			}
			fmt.Fprintf(&d, "M%d %dh%dv1h-%dz", x+quiet, y+quiet, run, run)
			x += run
		}
	}
	side := q.Size + 2*quiet
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" role="img" aria-label="%s" shape-rendering="crispEdges">`+
		`<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="%s"/></svg>`,
		side, side, escape(label), side, side, d.String())
}
