package piudoc

import "github.com/soypat/piudf/piupage"

// Family is one typeface at the weights and slants a document sets it in. Only
// Regular is required; the rest are what a bold or italic span falls back through.
type Family struct {
	Regular, Bold, Italic, BoldItalic piupage.Font
}

// pick returns the closest face the family carries to the weight and slant asked
// for, ending at Regular, which every family has.
func (fam Family) pick(bold, ital bool) piupage.Font {
	switch {
	case bold && ital && fam.BoldItalic != nil:
		return fam.BoldItalic
	case bold && fam.Bold != nil:
		return fam.Bold
	case ital && fam.Italic != nil:
		return fam.Italic
	}
	return fam.Regular
}

// namedFamily is one registered family. The name is what a [Style.Font] holds.
type namedFamily struct {
	name string
	fam  Family
}

// SetFamily registers fam under name, so a [Style] whose Font names it — or markup
// asking for bold or italic within it — is set in that face. Registering again
// under the same name replaces it; [Builder.Reset] keeps the registrations.
func (bld *Builder) SetFamily(name string, fam Family) {
	if name == "" || fam.Regular == nil {
		bld.fail(ErrBadFamily, 0, name)
		return
	}
	for i := range bld.parser.families {
		if bld.parser.families[i].name == name {
			bld.parser.families[i].fam = fam
			return
		}
	}
	bld.parser.families = append(bld.parser.families, namedFamily{name: name, fam: fam})
}

// resolveFont maps a family plus bold/italic flags to a registered face, falling
// back to the standard-14 font of that name when none is registered.
func (ap *atomParser) resolveFont(family string, bold, ital bool) piupage.Font {
	for i := range ap.families {
		if ap.families[i].name == family {
			return ap.families[i].fam.pick(bold, ital)
		}
	}
	if f, ok := piupage.Standard14(styleName(family, bold, ital)); ok {
		return f
	}
	return piupage.FontHelvetica
}
