package ppdf

// unfilterPNG reconstructs row cur in place from the row filter ft and the row
// above it (RFC 2083 section 6.3). The bytes-per-pixel is one: only 8-bit
// single-component rows reach here, which is every cross-reference stream.
//
// The filters chain — each row is expressed against the one above — which is
// why whoever reads them cannot skip.
func unfilterPNG(cur, prev []byte, ft byte) error {
	const bpp = 1
	for i := range cur {
		var a, c byte // Left and up-left reconstructed bytes.
		if i >= bpp {
			a = cur[i-bpp]
			c = prev[i-bpp]
		}
		b := prev[i] // Up.
		switch ft {
		case 0: // None.
		case 1: // Sub.
			cur[i] += a
		case 2: // Up.
			cur[i] += b
		case 3: // Average.
			cur[i] += byte((int(a) + int(b)) / 2)
		case 4: // Paeth.
			cur[i] += paeth(a, b, c)
		default:
			return errXrefStreamBad
		}
	}
	return nil
}

// paeth is the PNG Paeth predictor function (RFC 2083 section 6.6).
func paeth(a, b, c byte) byte {
	pa := int(b) - int(c)
	pb := int(a) - int(c)
	pc := pa + pb
	if pa < 0 {
		pa = -pa
	}
	if pb < 0 {
		pb = -pb
	}
	if pc < 0 {
		pc = -pc
	}
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}
