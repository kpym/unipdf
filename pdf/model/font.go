/*
 * This file is subject to the terms and conditions defined in
 * file 'LICENSE.md', which is part of this source code package.
 */

package model

import (
	"errors"
	"fmt"
	"strings"

	"github.com/unidoc/unidoc/common"
	"github.com/unidoc/unidoc/pdf/core"
	"github.com/unidoc/unidoc/pdf/internal/cmap"
	"github.com/unidoc/unidoc/pdf/model/fonts"
	"github.com/unidoc/unidoc/pdf/model/textencoding"
)

// PdfFont represents an underlying font structure which can be of type:
// - Type0
// - Type1
// - TrueType
// etc.
// It also holds the elements common to all fonts in fontSkeleton.
// XXX: The idea behind fontSkeleton is to avoid replicating the commmon font field parsing code
//      in all fonts. Is there a better way of doing this?
type PdfFont struct {
	fontSkeleton            // The fields common to all fonts
	context      fonts.Font // The underlying font: Type0, Type1, Truetype, etc..
}

// String returns a string that describes `font`.
func (font PdfFont) String() string {
	enc := ""
	if font.context.Encoder() != nil {
		enc = font.context.Encoder().String()
	}
	return fmt.Sprintf("FONT{%T %s %s}", font.context, font.fontSkeleton.coreString(), enc)
}

// BaseFont returns the font's "BaseFont" field.
func (font PdfFont) BaseFont() string {
	return font.basefont
}

// Subtype returns the font's "Subtype" field.
func (font PdfFont) Subtype() string {
	subtype := font.subtype
	if t, ok := font.context.(*pdfFontType0); ok {
		subtype = fmt.Sprintf("%s:%s", subtype, t.DescendantFont.Subtype())
	}
	return subtype
}

// ToUnicode returns the name of the font's "ToUnicode" field if there is one, or "" if there isn't.
func (font PdfFont) ToUnicode() string {
	if font.toUnicodeCmap == nil {
		return ""
	}
	return font.toUnicodeCmap.Name()
}

// NewStandard14Font returns the standard 14 font named `basefont` as a *PdfFont, or an error if it
// `basefont` is not one the standard 14 font names.
func NewStandard14Font(basefont string) (*PdfFont, error) {
	std, ok := fonts.Standard14Fonts[basefont]
	if !ok {
		return nil, ErrFontNotSupported
	}
	return &PdfFont{
		fontSkeleton: fontSkeleton{
			subtype:  "Type1",
			basefont: basefont,
		},
		context: std,
	}, nil
}

// NewPdfFontFromPdfObject loads a PdfFont from the dictionary `fontObj`.  If there is a problem an
// error is returned.
func NewPdfFontFromPdfObject(fontObj core.PdfObject) (*PdfFont, error) {
	return newPdfFontFromPdfObject(fontObj, true)
}

// newPdfFontFromPdfObject loads a PdfFont from the dictionary `fontObj`.  If there is a problem an
// error is returned.
// The allowType0 flag indicates whether loading Type0 font should be supported.  This is used to
// avoid cyclical loading.
func newPdfFontFromPdfObject(fontObj core.PdfObject, allowType0 bool) (*PdfFont, error) {
	skeleton, err := newFontSkeletonFromPdfObject(fontObj)
	if err != nil {
		return nil, err
	}
	font := &PdfFont{fontSkeleton: *skeleton}

	switch skeleton.subtype {
	case "Type0":
		if !allowType0 {
			common.Log.Debug("ERROR: Loading type0 not allowed. font=%s", skeleton)
			return nil, errors.New("Cyclical type0 loading")
		}
		type0font, err := newPdfFontType0FromPdfObject(skeleton)
		if err != nil {
			common.Log.Debug("ERROR: While loading Type0 font. font=%s err=%v", skeleton, err)
			return nil, err
		}
		font.context = type0font
	case "Type1", "Type3", "MMType1", "TrueType": // !@#$
		var simplefont *pdfFontSimple
		if std, ok := fonts.Standard14Fonts[font.basefont]; ok && font.subtype == "Type1" {
			font.context = std
			stdObj := core.TraceToDirectObject(std.ToPdfObject())
			stdSkeleton, err := newFontSkeletonFromPdfObject(stdObj)
			if err != nil {
				common.Log.Debug("ERROR: Bad Standard14\n\tfont=%s\n\tstd=%+v", font, std)
				return nil, err
			}
			simplefont, err = newSimpleFontFromPdfObject(stdSkeleton, true)
			if err != nil {
				common.Log.Debug("ERROR: Bad Standard14\n\tfont=%s\n\tstd=%+v", font, std)
				return nil, err
			}
		} else {
			simplefont, err = newSimpleFontFromPdfObject(skeleton, false)
			if err != nil {
				common.Log.Debug("ERROR: While loading simple font: font=%s err=%v", font, err)
				return nil, err
			}
		}
		err = simplefont.addEncoding()
		if err != nil {
			return nil, err
		}
		font.context = simplefont
	case "CIDFontType0":
		cidfont, err := newPdfCIDFontType0FromPdfObject(skeleton)
		if err != nil {
			common.Log.Debug("ERROR: While loading cid font type0 font: %v", err)
			return nil, err
		}
		font.context = cidfont
	case "CIDFontType2":
		cidfont, err := newPdfCIDFontType2FromPdfObject(skeleton)
		if err != nil {
			common.Log.Debug("ERROR: While loading cid font type2 font. font=%s err=%v", font, err)
			return nil, err
		}
		font.context = cidfont
	default:
		common.Log.Debug("ERROR: Unsupported font type: font=%s", font)
		return nil, fmt.Errorf("Unsupported font type: font=%s", font)
	}

	return font, nil
}

// CharcodeBytesToUnicode converts PDF character codes `data` to a Go unicode string.
//
// 9.10 Extraction of Text Content (page 292)
// The process of finding glyph descriptions in OpenType fonts by a conforming reader shall be the following:
// • For Type 1 fonts using “CFF” tables, the process shall be as described in 9.6.6.2, "Encodings
//   for Type 1 Fonts".
// • For TrueType fonts using “glyf” tables, the process shall be as described in 9.6.6.4,
//   "Encodings for TrueType Fonts". Since this process sometimes produces ambiguous results,
//   conforming writers, instead of using a simple font, shall use a Type 0 font with an Identity-H
//   encoding and use the glyph indices as character codes, as described following Table 118.
func (font PdfFont) CharcodeBytesToUnicode(data []byte) (string, int, int) {
	common.Log.Trace("showText: data=[% 02x]=%#q", data, data)

	charcodes := make([]uint16, 0, len(data)+len(data)%2)
	if font.isCIDFont() {
		if len(data) == 1 {
			data = []byte{0, data[0]}
		}
		if len(data)%2 != 0 {
			common.Log.Debug("ERROR: Padding data=%+v to even length", data)
			data = append(data, 0)
		}
		for i := 0; i < len(data); i += 2 {
			b := uint16(data[i])<<8 | uint16(data[i+1])
			charcodes = append(charcodes, b)
		}
	} else {
		for _, b := range data {
			charcodes = append(charcodes, uint16(b))
		}
	}

	charstrings := make([]string, 0, len(charcodes))
	numMisses := 0
	for _, code := range charcodes {
		if font.toUnicodeCmap != nil {
			r, ok := font.toUnicodeCmap.CharcodeToUnicode2(cmap.CharCode(code))
			if ok {
				charstrings = append(charstrings, r)
				continue
			}
		}
		// Fall back to encoding
		if encoder := font.Encoder(); encoder != nil {
			r, ok := encoder.CharcodeToRune(code)
			if ok {
				charstrings = append(charstrings, textencoding.RuneToString(r))
				continue
			}

			common.Log.Debug("ERROR: No rune. code=0x%04x data=[% 02x]=%#q charcodes=[% 04x] CID=%t\n"+
				"\tfont=%s\n\tencoding=%s",
				code, data, data, charcodes, font.isCIDFont(), font, encoder)
			numMisses++
			charstrings = append(charstrings, cmap.MissingCodeString)
		}
	}

	if numMisses != 0 {
		common.Log.Debug("ERROR: Couldn't convert to unicode. Using input. data=%#q=[% 02x]\n"+
			"\tnumChars=%d numMisses=%d\n"+
			"\tfont=%s",
			string(data), data, len(charcodes), numMisses, font)
	}

	out := strings.Join(charstrings, "")
	return out, len([]rune(out)), numMisses
}

// ToPdfObject converts the PdfFont object to its PDF representation.
func (font PdfFont) ToPdfObject() core.PdfObject {
	if t := font.actualFont(); t != nil {
		return t.ToPdfObject()
	}
	common.Log.Debug("ERROR: ToPdfObject Not implemented for font type=%#T. Returning null object",
		font.context)
	return core.MakeNull()
}

// Encoder returns the font's text encoder.
func (font PdfFont) Encoder() textencoding.TextEncoder {
	t := font.actualFont()
	if t == nil {
		common.Log.Debug("ERROR: Encoder not implemented for font type=%#T", font.context)
		// XXX: Should we return a default encoding?
		return nil
	}
	return t.Encoder()
}

// SetEncoder sets the encoding for the underlying font.
// !@#$ Is this only possible for simple fonts?
func (font PdfFont) SetEncoder(encoder textencoding.TextEncoder) {
	t := font.actualFont()
	if t == nil {
		common.Log.Debug("ERROR: SetEncoder. Not implemented for font type=%#T", font.context)
		return
	}
	t.SetEncoder(encoder)
}

// GetGlyphCharMetrics returns the specified char metrics for a specified glyph name.
func (font PdfFont) GetGlyphCharMetrics(glyph string) (fonts.CharMetrics, bool) {
	t := font.actualFont()
	if t == nil {
		common.Log.Debug("ERROR: GetGlyphCharMetrics Not implemented for font type=%#T", font.context)
		return fonts.CharMetrics{}, false
	}
	return t.GetGlyphCharMetrics(glyph)
}

// actualFont returns the Font in font.context
func (font PdfFont) actualFont() fonts.Font {
	if font.context == nil {
		common.Log.Debug("ERROR: actualFont. context is nil. font=%s", font)
	}
	switch t := font.context.(type) {
	case *pdfFontSimple:
		return t
	case *pdfFontType0:
		return t
	case *pdfCIDFontType0:
		return t
	case *pdfCIDFontType2:
		return t
	case fonts.FontCourier:
		return t
	case fonts.FontCourierBold:
		return t
	case fonts.FontCourierBoldOblique:
		return t
	case fonts.FontCourierOblique:
		return t
	case fonts.FontHelvetica:
		return t
	case fonts.FontHelveticaBold:
		return t
	case fonts.FontHelveticaBoldOblique:
		return t
	case fonts.FontHelveticaOblique:
		return t
	case fonts.FontTimesRoman:
		return t
	case fonts.FontTimesBold:
		return t
	case fonts.FontTimesBoldItalic:
		return t
	case fonts.FontTimesItalic:
		return t
	case fonts.FontSymbol:
		return t
	case fonts.FontZapfDingbats:
		return t
	default:
		common.Log.Debug("ERROR: actualFont. Unknown font type %t. font=%s", t, font)
		return nil
	}
}

// fontSkeleton represents the fields that are common to all PDF fonts.
type fontSkeleton struct {
	// All fonts have these fields
	basefont string // The font's "BaseFont" field.
	subtype  string // The font's "Subtype" field.

	// These are optional fields in the PDF font
	toUnicode core.PdfObject // The stream containing toUnicodeCmap. We keep it around for ToPdfObject.

	// These objects are computed from optional fields in the PDF font
	toUnicodeCmap  *cmap.CMap         // Computed from "ToUnicode"
	fontDescriptor *PdfFontDescriptor // Computed from "FontDescriptor"

	// This is an internal implementation detail. It is passed to specific font types so they can parse it.
	dict *core.PdfObjectDictionary

	// objectNumber helps us find the font in the PDF being processed. This helps with debugging
	objectNumber int64
}

// toFont returns a core.PdfObjectDictionary for `skel`.
// It is for use in font ToPdfObject functions.
// NOTE: The returned dict's "Subtype" field is set to `subtype` if `skel` doesn't have a subtype.
func (skel fontSkeleton) toDict(subtype string) *core.PdfObjectDictionary {

	if subtype != "" && skel.subtype != "" && subtype != skel.subtype {
		common.Log.Debug("ERROR: toDict. Overriding subtype to %#q %s", subtype, skel)
	} else if subtype == "" && skel.subtype == "" {
		common.Log.Debug("ERROR: toDict no subtype. font=%s", skel)
	} else if skel.subtype == "" {
		skel.subtype = subtype
	}

	d := core.MakeDict()
	d.Set("Type", core.MakeName("Font"))
	d.Set("BaseFont", core.MakeName(skel.basefont))
	d.Set("Subtype", core.MakeName(skel.subtype))

	if skel.fontDescriptor != nil {
		d.Set("FontDescriptor", skel.fontDescriptor.ToPdfObject())
	}
	if skel.toUnicode != nil {
		d.Set("ToUnicode", skel.toUnicode)
	}
	return d
}

// String returns a string that describes `skel`.
func (skel fontSkeleton) String() string {
	return fmt.Sprintf("FONT{%s}", skel.coreString())
}

func (skel fontSkeleton) coreString() string {
	descriptor := ""
	if skel.fontDescriptor != nil {
		descriptor = skel.fontDescriptor.String()
	}
	return fmt.Sprintf("%#q %#q obj=%d ToUnicode=%t %s",
		skel.subtype, skel.basefont, skel.objectNumber, skel.toUnicode != nil, descriptor)
}

// isCIDFont returns true if `skel` is a CID font.
func (skel fontSkeleton) isCIDFont() bool {
	if skel.subtype == "" {
		common.Log.Debug("ERROR: isCIDFont. context is nil. font=%s", skel)
	}
	isCID := false
	switch skel.subtype {
	case "Type0", "CIDFontType0", "CIDFontType2":
		isCID = true
	}
	common.Log.Trace("isCIDFont: isCID=%t font=%s", isCID, skel)
	return isCID
}

// newFontSkeletonFromPdfObject loads a fontSkeleton from a dictionary.  If there is a problem an
// error is returned.
// The fontSkeleton is the group of fields common to all PDF fonts.
func newFontSkeletonFromPdfObject(fontObj core.PdfObject) (*fontSkeleton, error) {
	font := &fontSkeleton{}

	if obj, ok := fontObj.(*core.PdfIndirectObject); ok {
		font.objectNumber = obj.ObjectNumber
	}

	dictObj := core.TraceToDirectObject(fontObj)

	d, ok := dictObj.(*core.PdfObjectDictionary)
	if !ok {
		if ref, ok := dictObj.(*core.PdfObjectReference); ok {
			common.Log.Debug("ERROR: Font is reference %s", ref)
		} else {
			common.Log.Debug("ERROR: Font not given by a dictionary (%T)", fontObj)
		}
		return nil, ErrFontNotSupported
	}
	font.dict = d

	objtype, err := core.GetName(core.TraceToDirectObject(d.Get("Type")))
	if err != nil {
		common.Log.Debug("ERROR: Font Incompatibility. Type (Required) missing")
		return nil, ErrRequiredAttributeMissing
	}
	if objtype != "Font" {
		common.Log.Debug("ERROR: Font Incompatibility. Type=%q. Should be %q.", objtype, "Font")
		return nil, core.ErrTypeError
	}

	subtype, err := core.GetName(core.TraceToDirectObject(d.Get("Subtype")))
	if err != nil {
		common.Log.Debug("ERROR: Font Incompatibility. Subtype (Required) missing")
		return nil, ErrRequiredAttributeMissing
	}
	font.subtype = subtype

	if subtype == "Type3" {
		common.Log.Debug("ERROR: Type 3 font not supprted. d=%s", d)
		return nil, ErrFontNotSupported
	}

	basefont, err := core.GetName(core.TraceToDirectObject(d.Get("BaseFont")))
	if err != nil {
		common.Log.Debug("ERROR: Font Incompatibility. BaseFont (Required) missing")
		return nil, ErrRequiredAttributeMissing
	}
	font.basefont = basefont

	obj := d.Get("FontDescriptor")
	if obj != nil {
		fontDescriptor, err := newPdfFontDescriptorFromPdfObject(obj)
		if err != nil {
			common.Log.Debug("ERROR: Bad font descriptor. err=%v", err)
			return nil, err
		}
		font.fontDescriptor = fontDescriptor
	}

	toUnicode := d.Get("ToUnicode")
	if toUnicode != nil {
		font.toUnicode = core.TraceToDirectObject(toUnicode)
		codemap, err := toUnicodeToCmap(font.toUnicode, font)
		if err != nil {
			return nil, err
		}
		font.toUnicodeCmap = codemap
	}

	return font, nil
}

// toUnicodeToCmap returns a CMap of `toUnicode` if it exists
func toUnicodeToCmap(toUnicode core.PdfObject, font *fontSkeleton) (*cmap.CMap, error) {
	toUnicodeStream, ok := toUnicode.(*core.PdfObjectStream)
	if !ok {
		common.Log.Debug("ERROR: toUnicodeToCmap: Not a stream (%T)", toUnicode)
		return nil, core.ErrTypeError
	}
	data, err := core.DecodeStream(toUnicodeStream)
	if err != nil {
		return nil, err
	}

	cm, err := cmap.LoadCmapFromData(data, !font.isCIDFont())
	if err != nil {
		// Show the object number of the bad cmap to help with debugging.
		common.Log.Debug("ERROR: ObjectNumber=%d err=%v", toUnicodeStream.ObjectNumber, err)
	}
	return cm, err
}

// PdfFontDescriptor specifies metrics and other attributes of a font and can refer to a FontFile
// for embedded fonts.
// 9.8 Font Descriptors (page 281)
type PdfFontDescriptor struct {
	FontName     core.PdfObject
	FontFamily   core.PdfObject
	FontStretch  core.PdfObject
	FontWeight   core.PdfObject
	Flags        core.PdfObject
	FontBBox     core.PdfObject
	ItalicAngle  core.PdfObject
	Ascent       core.PdfObject
	Descent      core.PdfObject
	Leading      core.PdfObject
	CapHeight    core.PdfObject
	XHeight      core.PdfObject
	StemV        core.PdfObject
	StemH        core.PdfObject
	AvgWidth     core.PdfObject
	MaxWidth     core.PdfObject
	MissingWidth core.PdfObject
	FontFile     core.PdfObject // PFB
	FontFile2    core.PdfObject // TTF
	FontFile3    core.PdfObject // OTF / CFF
	CharSet      core.PdfObject

	*fontFile
	fontFile2 *fonts.TtfType

	// Additional entries for CIDFonts
	Style  core.PdfObject
	Lang   core.PdfObject
	FD     core.PdfObject
	CIDSet core.PdfObject

	// Container.
	container *core.PdfIndirectObject
}

// String returns a string describing the font descriptor.
func (descriptor *PdfFontDescriptor) String() string {
	parts := []string{}
	if descriptor.FontName != nil {
		parts = append(parts, descriptor.FontName.String())
	}
	if descriptor.FontFamily != nil {
		parts = append(parts, descriptor.FontFamily.String())
	}
	if descriptor.fontFile != nil {
		parts = append(parts, descriptor.fontFile.String())
	}
	if descriptor.fontFile2 != nil {
		parts = append(parts, descriptor.fontFile2.String())
	}
	parts = append(parts, fmt.Sprintf("FontFile3=%t", descriptor.FontFile3 != nil))

	return fmt.Sprintf("FONT_DESCRIPTON{%s}", strings.Join(parts, ", "))
}

// newPdfFontDescriptorFromPdfObject loads the font descriptor from a core.PdfObject.  Can either be a
// *PdfIndirectObject or a *core.PdfObjectDictionary.
func newPdfFontDescriptorFromPdfObject(obj core.PdfObject) (*PdfFontDescriptor, error) {
	descriptor := &PdfFontDescriptor{}

	if ind, is := obj.(*core.PdfIndirectObject); is {
		descriptor.container = ind
		obj = ind.PdfObject
	}

	d, ok := obj.(*core.PdfObjectDictionary)
	if !ok {
		common.Log.Debug("ERROR: FontDescriptor not given by a dictionary (%T)", obj)
		return nil, core.ErrTypeError
	}

	if obj := d.Get("FontName"); obj != nil {
		descriptor.FontName = obj
	} else {
		common.Log.Debug("Incompatibility: FontName (Required) missing")
	}
	fontname, _ := core.GetName(descriptor.FontName)

	if obj := d.Get("Type"); obj != nil {
		oname, is := obj.(*core.PdfObjectName)
		if !is || string(*oname) != "FontDescriptor" {
			common.Log.Debug("Incompatibility: Font descriptor Type invalid (%T) font=%q %T",
				obj, fontname, descriptor.FontName)
		}
	} else {
		common.Log.Trace("Incompatibility: Type (Required) missing. font=%q %T",
			fontname, descriptor.FontName)
	}

	descriptor.FontFamily = d.Get("FontFamily")
	descriptor.FontStretch = d.Get("FontStretch")
	descriptor.FontWeight = d.Get("FontWeight")
	descriptor.Flags = d.Get("Flags")
	descriptor.FontBBox = d.Get("FontBBox")
	descriptor.ItalicAngle = d.Get("ItalicAngle")
	descriptor.Ascent = d.Get("Ascent")
	descriptor.Descent = d.Get("Descent")
	descriptor.Leading = d.Get("Leading")
	descriptor.CapHeight = d.Get("CapHeight")
	descriptor.XHeight = d.Get("XHeight")
	descriptor.StemV = d.Get("StemV")
	descriptor.StemH = d.Get("StemH")
	descriptor.AvgWidth = d.Get("AvgWidth")
	descriptor.MaxWidth = d.Get("MaxWidth")
	descriptor.MissingWidth = d.Get("MissingWidth")
	descriptor.FontFile = d.Get("FontFile")
	descriptor.FontFile2 = d.Get("FontFile2")
	descriptor.FontFile3 = d.Get("FontFile3")
	descriptor.CharSet = d.Get("CharSet")
	descriptor.Style = d.Get("Style")
	descriptor.Lang = d.Get("Lang")
	descriptor.FD = d.Get("FD")
	descriptor.CIDSet = d.Get("CIDSet")

	if descriptor.FontFile != nil {
		fontFile, err := newFontFileFromPdfObject(descriptor.FontFile)
		if err != nil {
			return descriptor, err
		}
		common.Log.Trace("fontFile=%s", fontFile)
		descriptor.fontFile = fontFile
	}
	if descriptor.FontFile2 != nil {
		fontFile2, err := fonts.NewFontFile2FromPdfObject(descriptor.FontFile2)
		if err != nil {
			return descriptor, err
		}
		common.Log.Trace("fontFile2=%s", fontFile2.String())
		descriptor.fontFile2 = &fontFile2
	}
	return descriptor, nil
}

// ToPdfObject returns the PdfFontDescriptor as a PDF dictionary inside an indirect object.
func (this *PdfFontDescriptor) ToPdfObject() core.PdfObject {
	d := core.MakeDict()
	if this.container == nil {
		this.container = &core.PdfIndirectObject{}
	}
	this.container.PdfObject = d

	d.Set("Type", core.MakeName("FontDescriptor"))

	if this.FontName != nil {
		d.Set("FontName", this.FontName)
	}

	if this.FontFamily != nil {
		d.Set("FontFamily", this.FontFamily)
	}

	if this.FontStretch != nil {
		d.Set("FontStretch", this.FontStretch)
	}

	if this.FontWeight != nil {
		d.Set("FontWeight", this.FontWeight)
	}

	if this.Flags != nil {
		d.Set("Flags", this.Flags)
	}

	if this.FontBBox != nil {
		d.Set("FontBBox", this.FontBBox)
	}

	if this.ItalicAngle != nil {
		d.Set("ItalicAngle", this.ItalicAngle)
	}

	if this.Ascent != nil {
		d.Set("Ascent", this.Ascent)
	}

	if this.Descent != nil {
		d.Set("Descent", this.Descent)
	}

	if this.Leading != nil {
		d.Set("Leading", this.Leading)
	}

	if this.CapHeight != nil {
		d.Set("CapHeight", this.CapHeight)
	}

	if this.XHeight != nil {
		d.Set("XHeight", this.XHeight)
	}

	if this.StemV != nil {
		d.Set("StemV", this.StemV)
	}

	if this.StemH != nil {
		d.Set("StemH", this.StemH)
	}

	if this.AvgWidth != nil {
		d.Set("AvgWidth", this.AvgWidth)
	}

	if this.MaxWidth != nil {
		d.Set("MaxWidth", this.MaxWidth)
	}

	if this.MissingWidth != nil {
		d.Set("MissingWidth", this.MissingWidth)
	}

	if this.FontFile != nil {
		d.Set("FontFile", this.FontFile)
	}

	if this.FontFile2 != nil {
		d.Set("FontFile2", this.FontFile2)
	}

	if this.FontFile3 != nil {
		d.Set("FontFile3", this.FontFile3)
	}

	if this.CharSet != nil {
		d.Set("CharSet", this.CharSet)
	}

	if this.Style != nil {
		d.Set("FontName", this.FontName)
	}

	if this.Lang != nil {
		d.Set("Lang", this.Lang)
	}

	if this.FD != nil {
		d.Set("FD", this.FD)
	}

	if this.CIDSet != nil {
		d.Set("CIDSet", this.CIDSet)
	}

	return this.container
}
