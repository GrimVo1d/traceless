package exif

import "fmt"

// tagName returns the name of a tag, or its number if it is not listed.
func tagName(k kind, tag uint16) string {
	names := mainTags
	switch k {
	case kindGPS:
		names = gpsTags
	case kindInterop:
		names = interopTags
	}
	if name, ok := names[tag]; ok {
		return name
	}
	return fmt.Sprintf("Tag 0x%04X", tag)
}

// mainTags names TIFF and Exif tags. Unlisted tags are still reported, by
// number.
var mainTags = map[uint16]string{
	// TIFF
	0x00fe: "SubfileType",
	0x0100: "ImageWidth",
	0x0101: "ImageLength",
	0x0102: "BitsPerSample",
	0x0103: "Compression",
	0x0106: "PhotometricInterpretation",
	0x010e: "ImageDescription",
	0x010f: "Make",
	0x0110: "Model",
	0x0112: "Orientation",
	0x0115: "SamplesPerPixel",
	0x0116: "RowsPerStrip",
	0x0117: "StripByteCounts",
	0x011a: "XResolution",
	0x011b: "YResolution",
	0x011c: "PlanarConfiguration",
	0x0128: "ResolutionUnit",
	0x0131: "Software",
	0x0132: "DateTime",
	0x013b: "Artist",
	0x013c: "HostComputer",
	0x0211: "YCbCrCoefficients",
	0x0212: "YCbCrSubSampling",
	0x0213: "YCbCrPositioning",
	0x0214: "ReferenceBlackWhite",
	0x8298: "Copyright",
	0x9c9b: "XPTitle",
	0x9c9c: "XPComment",
	0x9c9d: "XPAuthor",
	0x9c9e: "XPKeywords",
	0x9c9f: "XPSubject",

	// Exif
	0x829a: "ExposureTime",
	0x829d: "FNumber",
	0x8822: "ExposureProgram",
	0x8827: "ISO",
	0x8830: "SensitivityType",
	0x9000: "ExifVersion",
	0x9003: "DateTimeOriginal",
	0x9004: "DateTimeDigitized",
	0x9010: "OffsetTime",
	0x9011: "OffsetTimeOriginal",
	0x9012: "OffsetTimeDigitized",
	0x9101: "ComponentsConfiguration",
	0x9201: "ShutterSpeedValue",
	0x9202: "ApertureValue",
	0x9203: "BrightnessValue",
	0x9204: "ExposureBiasValue",
	0x9205: "MaxApertureValue",
	0x9206: "SubjectDistance",
	0x9207: "MeteringMode",
	0x9208: "LightSource",
	0x9209: "Flash",
	0x920a: "FocalLength",
	0x9214: "SubjectArea",
	0x927c: "MakerNote",
	0x9286: "UserComment",
	0x9290: "SubSecTime",
	0x9291: "SubSecTimeOriginal",
	0x9292: "SubSecTimeDigitized",
	0xa000: "FlashpixVersion",
	0xa001: "ColorSpace",
	0xa002: "PixelXDimension",
	0xa003: "PixelYDimension",
	0xa004: "RelatedSoundFile",
	0xa20e: "FocalPlaneXResolution",
	0xa20f: "FocalPlaneYResolution",
	0xa210: "FocalPlaneResolutionUnit",
	0xa217: "SensingMethod",
	0xa300: "FileSource",
	0xa301: "SceneType",
	0xa401: "CustomRendered",
	0xa402: "ExposureMode",
	0xa403: "WhiteBalance",
	0xa404: "DigitalZoomRatio",
	0xa405: "FocalLengthIn35mmFilm",
	0xa406: "SceneCaptureType",
	0xa408: "Contrast",
	0xa409: "Saturation",
	0xa40a: "Sharpness",
	0xa40c: "SubjectDistanceRange",
	0xa420: "ImageUniqueID",
	0xa430: "CameraOwnerName",
	0xa431: "BodySerialNumber",
	0xa432: "LensSpecification",
	0xa433: "LensMake",
	0xa434: "LensModel",
	0xa435: "LensSerialNumber",
	0xa460: "CompositeImage",
}

var gpsTags = map[uint16]string{
	0x00: "GPSVersionID",
	0x01: "GPSLatitudeRef",
	0x02: "GPSLatitude",
	0x03: "GPSLongitudeRef",
	0x04: "GPSLongitude",
	0x05: "GPSAltitudeRef",
	0x06: "GPSAltitude",
	0x07: "GPSTimeStamp",
	0x08: "GPSSatellites",
	0x09: "GPSStatus",
	0x0a: "GPSMeasureMode",
	0x0b: "GPSDOP",
	0x0c: "GPSSpeedRef",
	0x0d: "GPSSpeed",
	0x0e: "GPSTrackRef",
	0x0f: "GPSTrack",
	0x10: "GPSImgDirectionRef",
	0x11: "GPSImgDirection",
	0x12: "GPSMapDatum",
	0x13: "GPSDestLatitudeRef",
	0x14: "GPSDestLatitude",
	0x15: "GPSDestLongitudeRef",
	0x16: "GPSDestLongitude",
	0x17: "GPSDestBearingRef",
	0x18: "GPSDestBearing",
	0x19: "GPSDestDistanceRef",
	0x1a: "GPSDestDistance",
	0x1b: "GPSProcessingMethod",
	0x1c: "GPSAreaInformation",
	0x1d: "GPSDateStamp",
	0x1e: "GPSDifferential",
	0x1f: "GPSHPositioningError",
}

var interopTags = map[uint16]string{
	0x0001: "InteropIndex",
	0x0002: "InteropVersion",
}
