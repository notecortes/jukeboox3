package scanner

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// RawMetadata contiene los datos leídos directamente de las etiquetas embebidas del archivo de audio.
type RawMetadata struct {
	Title       string
	Artist      string
	Album       string
	AlbumArtist string
	Genre       string
	Year        int
	TrackNumber int
	Duration    float64
	Bitrate     int
	SampleRate  int
}

// ReadAudioTags lee etiquetas ID3v2 (MP3) o Vorbis Comments (FLAC/OGG) y propiedades de audio nativas en Go.
func ReadAudioTags(filePath string) *RawMetadata {
	f, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var fileSize int64
	if fi, err := f.Stat(); err == nil {
		fileSize = fi.Size()
	}

	ext := strings.ToLower(filePath)
	if strings.HasSuffix(ext, ".flac") {
		return readFLACTags(f, fileSize)
	} else if strings.HasSuffix(ext, ".mp3") {
		return readID3v2Tags(f, fileSize)
	} else if strings.HasSuffix(ext, ".wav") {
		dur, br, sr := readWAVProperties(f, fileSize)
		return &RawMetadata{
			Duration:   dur,
			Bitrate:    br,
			SampleRate: sr,
		}
	}
	return nil
}

// readFLACTags decodifica bloques de comentarios Vorbis y bloque STREAMINFO para duración y calidad en archivos FLAC.
func readFLACTags(r io.ReadSeeker, fileSize int64) *RawMetadata {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil || string(header) != "fLaC" {
		return nil
	}

	meta := &RawMetadata{
		SampleRate: 44100,
		Bitrate:    960,
	}
	blockHeader := make([]byte, 4)
	gotStreamInfo := false
	gotVorbis := false

	for {
		if _, err := io.ReadFull(r, blockHeader); err != nil {
			break
		}
		isLast := (blockHeader[0] & 0x80) != 0
		blockType := blockHeader[0] & 0x7F
		length := int(blockHeader[1])<<16 | int(blockHeader[2])<<8 | int(blockHeader[3])

		// Bloque 0: STREAMINFO (duración exacta, sample rate, canales, bits)
		if blockType == 0 && length >= 34 && length <= 65536 {
			infoData := make([]byte, 34)
			if _, err := io.ReadFull(r, infoData); err != nil {
				break
			}
			if length > 34 {
				if _, err := r.Seek(int64(length-34), io.SeekCurrent); err != nil {
					break
				}
			}
			sr := uint32(infoData[10])<<12 | uint32(infoData[11])<<4 | uint32(infoData[12])>>4
			totalSamples := (uint64(infoData[13]&0x0F) << 32) | (uint64(infoData[14]) << 24) |
				(uint64(infoData[15]) << 16) | (uint64(infoData[16]) << 8) | uint64(infoData[17])
			if sr > 0 {
				meta.SampleRate = int(sr)
				if totalSamples > 0 {
					meta.Duration = float64(totalSamples) / float64(sr)
					if fileSize > 0 && meta.Duration > 0 {
						meta.Bitrate = int((float64(fileSize) * 8.0) / meta.Duration / 1000.0)
					}
				}
			}
			gotStreamInfo = true
		} else if blockType == 4 && length > 0 && length <= 2*1024*1024 { // Bloque 4: Vorbis Comment
			blockData := make([]byte, length)
			if _, err := io.ReadFull(r, blockData); err != nil {
				break
			}
			parseVorbisComments(blockData, meta)
			gotVorbis = true
		} else {
			// Saltar otros bloques (ej. PICTURE / PADDING) sin leer bytes de red por SMB
			if _, err := r.Seek(int64(length), io.SeekCurrent); err != nil {
				break
			}
		}

		if isLast || (gotStreamInfo && gotVorbis) {
			break
		}
	}

	if meta.Title != "" || meta.Artist != "" || meta.Album != "" || meta.Duration > 0 {
		return meta
	}
	return nil
}

// parseVorbisComments parsea los pares CLAVE=VALOR del estándar Vorbis.
func parseVorbisComments(data []byte, meta *RawMetadata) {
	if len(data) < 8 {
		return
	}
	reader := bytes.NewReader(data)

	// Vendor length & string
	var vendorLen uint32
	if err := binary.Read(reader, binary.LittleEndian, &vendorLen); err != nil || int64(vendorLen) > int64(len(data)) {
		return
	}
	if _, err := reader.Seek(int64(vendorLen), io.SeekCurrent); err != nil {
		return
	}

	// Número de comentarios
	var numComments uint32
	if err := binary.Read(reader, binary.LittleEndian, &numComments); err != nil {
		return
	}
	if numComments > 4096 {
		numComments = 4096
	}

	for i := uint32(0); i < numComments; i++ {
		var commentLen uint32
		if err := binary.Read(reader, binary.LittleEndian, &commentLen); err != nil {
			break
		}
		if commentLen > 4096 {
			// Protección contra longitudes anómalas
			if _, err := reader.Seek(int64(commentLen), io.SeekCurrent); err != nil {
				break
			}
			continue
		}
		commentBuf := make([]byte, commentLen)
		if _, err := io.ReadFull(reader, commentBuf); err != nil {
			break
		}
		commentStr := string(commentBuf)
		parts := strings.SplitN(commentStr, "=", 2)
		if len(parts) == 2 {
			k := strings.ToUpper(strings.TrimSpace(parts[0]))
			v := strings.TrimSpace(parts[1])
			switch k {
			case "TITLE":
				meta.Title = v
			case "ARTIST":
				meta.Artist = v
			case "ALBUM":
				meta.Album = v
			case "ALBUMARTIST", "ALBUM ARTIST":
				meta.AlbumArtist = v
			case "GENRE":
				meta.Genre = v
			case "DATE", "YEAR":
				if len(v) >= 4 {
					if y, err := strconv.Atoi(v[:4]); err == nil {
						meta.Year = y
					}
				}
			case "TRACKNUMBER", "TRACK":
				tParts := strings.Split(v, "/")
				if num, err := strconv.Atoi(tParts[0]); err == nil {
					meta.TrackNumber = num
				}
			}
		}
	}
}

// isWantedID3Frame indica si el frame ID3v2 contiene metadatos de texto útiles y debe leerse.
func isWantedID3Frame(frameID string) bool {
	switch frameID {
	case "TIT2", "TT2", "TPE1", "TP1", "TALB", "TAL", "TPE2", "TP2",
		"TCON", "TCO", "TYER", "TYE", "TDRC", "TRCK", "TRK", "TLEN", "TLE":
		return true
	}
	return false
}

// isValidFrameID comprueba que el identificador de frame ID3 esté compuesto por letras mayúsculas o dígitos ASCII.
func isValidFrameID(id string) bool {
	if len(id) == 0 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// applyID3TextFrame asigna el valor decodificado de un frame ID3 al struct RawMetadata.
func applyID3TextFrame(frameID string, frameBody []byte, meta *RawMetadata) {
	val := decodeTextFrame(frameBody)
	if val == "" {
		return
	}
	switch frameID {
	case "TIT2", "TT2":
		meta.Title = val
	case "TPE1", "TP1":
		meta.Artist = val
	case "TALB", "TAL":
		meta.Album = val
	case "TPE2", "TP2":
		meta.AlbumArtist = val
	case "TCON", "TCO":
		meta.Genre = val
	case "TYER", "TYE", "TDRC":
		if len(val) >= 4 {
			if y, err := strconv.Atoi(val[:4]); err == nil {
				meta.Year = y
			}
		}
	case "TRCK", "TRK":
		tParts := strings.Split(val, "/")
		if num, err := strconv.Atoi(tParts[0]); err == nil {
			meta.TrackNumber = num
		}
	case "TLEN", "TLE": // Duración en milisegundos en ID3v2
		if ms, err := strconv.ParseFloat(val, 64); err == nil && ms > 0 {
			meta.Duration = ms / 1000.0
		}
	}
}

// parseID3FrameHeader extrae el identificador y tamaño de un frame ID3v2 (v2.2, v2.3 o v2.4).
func parseID3FrameHeader(hdr []byte, version byte) (string, int) {
	if version == 2 {
		if len(hdr) < 6 {
			return "", 0
		}
		return string(hdr[:3]), int(hdr[3])<<16 | int(hdr[4])<<8 | int(hdr[5])
	}
	if len(hdr) < 10 {
		return "", 0
	}
	frameID := string(hdr[:4])
	var frameSize int
	if version == 4 && (hdr[4]|hdr[5]|hdr[6]|hdr[7])&0x80 == 0 {
		frameSize = syncSafeInt(hdr[4:8])
	} else {
		frameSize = int(binary.BigEndian.Uint32(hdr[4:8]))
	}
	return frameID, frameSize
}

// streamID3v2Frames recorre un bloque ID3v2 grande saltando por Seek los frames binarios (APIC, PRIV, GEOB).
func streamID3v2Frames(r io.ReadSeeker, tagSize int, version byte, meta *RawMetadata) {
	hdrLen := 10
	if version == 2 {
		hdrLen = 6
	}
	frameHdr := make([]byte, hdrLen)
	pos := 0

	for pos+hdrLen <= tagSize {
		if _, err := io.ReadFull(r, frameHdr); err != nil {
			break
		}
		pos += hdrLen

		if frameHdr[0] == 0 {
			break
		}

		frameID, frameSize := parseID3FrameHeader(frameHdr, version)
		if !isValidFrameID(frameID) || frameSize <= 0 || int64(pos)+int64(frameSize) > int64(tagSize) {
			break
		}

		if isWantedID3Frame(frameID) && frameSize <= 8192 {
			frameBody := make([]byte, frameSize)
			if _, err := io.ReadFull(r, frameBody); err != nil {
				break
			}
			applyID3TextFrame(frameID, frameBody, meta)
		} else {
			if _, err := r.Seek(int64(frameSize), io.SeekCurrent); err != nil {
				break
			}
		}
		pos += frameSize
	}
}

// readID3v2Tags decodifica frames estándar de ID3v2 en MP3 y analiza frames MPEG para calcular la duración exacta.
func readID3v2Tags(r io.ReadSeeker, fileSize int64) *RawMetadata {
	meta := &RawMetadata{
		SampleRate: 44100,
		Bitrate:    320,
	}

	header := make([]byte, 10)
	var audioStartOffset int64 = 0

	if _, err := io.ReadFull(r, header); err == nil && string(header[:3]) == "ID3" {
		version := header[3]
		flags := header[5]
		tagSize := syncSafeInt(header[6:10])
		if version >= 2 && version <= 4 && tagSize > 0 && tagSize <= 20*1024*1024 {
			audioStartOffset = int64(10) + int64(tagSize)

			// Saltar cabecera extendida opcional (bit 6) si existe
			extSkip := 0
			if (version == 3 || version == 4) && (flags&0x40) != 0 && tagSize > 4 {
				extHdr := make([]byte, 4)
				if _, err := io.ReadFull(r, extHdr); err == nil {
					if version == 4 {
						extSize := syncSafeInt(extHdr)
						if extSize > 4 && extSize <= tagSize {
							_, _ = r.Seek(int64(extSize-4), io.SeekCurrent)
							extSkip = extSize
						}
					} else {
						extSize := int(binary.BigEndian.Uint32(extHdr))
						if extSize >= 0 && extSize+4 <= tagSize {
							_, _ = r.Seek(int64(extSize), io.SeekCurrent)
							extSkip = extSize + 4
						}
					}
				}
			}

			remainingTagSize := tagSize - extSkip
			if remainingTagSize > 0 && remainingTagSize <= 65536 {
				// Lectura única en memoria para cabeceras ligeras sin carátula grande
				tagData := make([]byte, remainingTagSize)
				if _, err := io.ReadFull(r, tagData); err == nil {
					hdrLen := 10
					if version == 2 {
						hdrLen = 6
					}
					offset := 0
					for offset+hdrLen <= len(tagData) {
						if tagData[offset] == 0 {
							break
						}
						frameID, frameSize := parseID3FrameHeader(tagData[offset:offset+hdrLen], version)
						offset += hdrLen
						if !isValidFrameID(frameID) || frameSize <= 0 || offset+frameSize > len(tagData) {
							break
						}
						frameBody := tagData[offset : offset+frameSize]
						offset += frameSize
						if isWantedID3Frame(frameID) && frameSize <= 8192 {
							applyID3TextFrame(frameID, frameBody, meta)
						}
					}
				}
			} else if remainingTagSize > 65536 {
				// Streaming por Seek para saltar carátulas APIC pesadas sin descargarlas por SMB
				streamID3v2Frames(r, remainingTagSize, version, meta)
			}
		}
	}

	// Si no vino TLEN en las etiquetas ID3, calcular duración real inspeccionando los frames MPEG
	if meta.Duration <= 0 {
		dur, br, sr := readMPEGFrameProperties(r, fileSize, audioStartOffset)
		if dur > 0 {
			meta.Duration = dur
		}
		if br > 0 {
			meta.Bitrate = br
		}
		if sr > 0 {
			meta.SampleRate = sr
		}
	}

	if meta.Title != "" || meta.Artist != "" || meta.Album != "" || meta.Duration > 0 {
		return meta
	}
	return nil
}

// readMPEGFrameProperties busca el primer frame MPEG de audio válido y calcula la duración, bitrate y sample rate.
func readMPEGFrameProperties(r io.ReadSeeker, fileSize int64, audioStartOffset int64) (float64, int, int) {
	if fileSize <= audioStartOffset {
		return 0, 0, 0
	}
	if _, err := r.Seek(audioStartOffset, io.SeekStart); err != nil {
		return 0, 0, 0
	}

	buf := make([]byte, 16384)
	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return 0, 0, 0
	}
	if n < 4 {
		return 0, 0, 0
	}

	mp1Bitrates := []int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	mp2Bitrates := []int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
	mp1SampleRates := []int{44100, 48000, 32000, 0}
	mp2SampleRates := []int{22050, 24000, 16000, 0}
	mp25SampleRates := []int{11025, 12000, 8000, 0}

	for i := 0; i <= n-4; i++ {
		// Buscar cabecera de sincronización 0xFF 0xEx (11 bits consecutivos a 1)
		if buf[i] != 0xFF || (buf[i+1]&0xE0) != 0xE0 {
			continue
		}

		b2 := buf[i+1]
		b3 := buf[i+2]
		b4 := buf[i+3]

		versionBits := (b2 >> 3) & 0x03 // 0=2.5, 2=2, 3=1
		layerBits := (b2 >> 1) & 0x03   // 1=Layer III, 2=Layer II, 3=Layer I
		if versionBits == 1 || layerBits == 0 {
			continue
		}

		bitrateIdx := int((b3 >> 4) & 0x0F)
		srIdx := int((b3 >> 2) & 0x03)
		if bitrateIdx == 0 || bitrateIdx == 15 || srIdx == 3 {
			continue
		}

		var detectedBitrate int
		var detectedSampleRate int

		if versionBits == 3 { // MPEG-1
			if layerBits == 1 {
				detectedBitrate = mp1Bitrates[bitrateIdx]
			}
			detectedSampleRate = mp1SampleRates[srIdx]
		} else if versionBits == 2 { // MPEG-2
			if layerBits == 1 {
				detectedBitrate = mp2Bitrates[bitrateIdx]
			}
			detectedSampleRate = mp2SampleRates[srIdx]
		} else if versionBits == 0 { // MPEG-2.5
			if layerBits == 1 {
				detectedBitrate = mp2Bitrates[bitrateIdx]
			}
			detectedSampleRate = mp25SampleRates[srIdx]
		}

		if detectedBitrate <= 0 || detectedSampleRate <= 0 {
			continue
		}

		// Comprobar cabecera Xing / Info para archivos VBR
		channelMode := (b4 >> 6) & 0x03
		xingOffset := i + 36
		if versionBits == 3 && channelMode == 3 {
			xingOffset = i + 21
		} else if versionBits != 3 {
			if channelMode == 3 {
				xingOffset = i + 13
			} else {
				xingOffset = i + 21
			}
		}

		if xingOffset+8 <= n {
			headerTag := string(buf[xingOffset : xingOffset+4])
			if headerTag == "Xing" || headerTag == "Info" {
				flags := binary.BigEndian.Uint32(buf[xingOffset+4 : xingOffset+8])
				if (flags&0x01) != 0 && xingOffset+12 <= n {
					frameCount := binary.BigEndian.Uint32(buf[xingOffset+8 : xingOffset+12])
					samplesPerFrame := 1152
					if versionBits != 3 {
						samplesPerFrame = 576
					}
					dur := float64(frameCount*uint32(samplesPerFrame)) / float64(detectedSampleRate)
					if dur > 0 {
						if (flags&0x02) != 0 && xingOffset+16 <= n {
							bytesCount := binary.BigEndian.Uint32(buf[xingOffset+12 : xingOffset+16])
							detectedBitrate = int(float64(bytesCount*8) / dur / 1000.0)
						}
						return dur, detectedBitrate, detectedSampleRate
					}
				}
			}
		}

		// Comprobar cabecera VBRI (Fraunhofer)
		vbriOffset := i + 36
		if vbriOffset+18 <= n && string(buf[vbriOffset:vbriOffset+4]) == "VBRI" {
			frameCount := binary.BigEndian.Uint32(buf[vbriOffset+14 : vbriOffset+18])
			dur := float64(frameCount*1152) / float64(detectedSampleRate)
			if dur > 0 {
				return dur, detectedBitrate, detectedSampleRate
			}
		}

		// Si es CBR estándar sin cabecera VBR:
		audioBytes := fileSize - audioStartOffset
		if audioBytes > 128 {
			audioBytes -= 128 // descuento de posible tag ID3v1 final
		}
		if audioBytes > 0 && detectedBitrate > 0 {
			dur := float64(audioBytes*8) / float64(detectedBitrate*1000)
			return dur, detectedBitrate, detectedSampleRate
		}
	}

	return 0, 0, 0
}

// readWAVProperties extrae duración, bitrate y sample rate de archivos WAV.
func readWAVProperties(r io.Reader, fileSize int64) (float64, int, int) {
	header := make([]byte, 44)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, 0, 0
	}
	if string(header[:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return 0, 0, 0
	}
	sr := int(binary.LittleEndian.Uint32(header[24:28]))
	byteRate := int(binary.LittleEndian.Uint32(header[28:32]))
	dataSize := int64(binary.LittleEndian.Uint32(header[40:44]))
	if dataSize <= 0 && fileSize > 44 {
		dataSize = fileSize - 44
	}
	if byteRate > 0 && dataSize > 0 {
		dur := float64(dataSize) / float64(byteRate)
		br := int(float64(byteRate*8) / 1000.0)
		return dur, br, sr
	}
	return 0, 1411, sr
}

// ReadAudioProperties analiza de forma ultra-rápida un archivo de audio para calcular duración, bitrate y sample rate.
func ReadAudioProperties(filePath string) (float64, int, int, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return 0, 0, 0, err
	}
	fileSize := fi.Size()

	ext := strings.ToLower(filePath)
	if strings.HasSuffix(ext, ".flac") {
		meta := readFLACTags(f, fileSize)
		if meta != nil && meta.Duration > 0 {
			return meta.Duration, meta.Bitrate, meta.SampleRate, nil
		}
	} else if strings.HasSuffix(ext, ".mp3") {
		meta := readID3v2Tags(f, fileSize)
		if meta != nil && meta.Duration > 0 {
			return meta.Duration, meta.Bitrate, meta.SampleRate, nil
		}
		_, _ = f.Seek(0, io.SeekStart)
		dur, br, sr := readMPEGFrameProperties(f, fileSize, 0)
		return dur, br, sr, nil
	} else if strings.HasSuffix(ext, ".wav") {
		dur, br, sr := readWAVProperties(f, fileSize)
		return dur, br, sr, nil
	}
	return 0, 0, 0, nil
}

// decodeTextFrame decodifica la cadena de texto con codificación ISO-8859-1, UTF-16 o UTF-8.
func decodeTextFrame(data []byte) string {
	if len(data) <= 1 {
		return ""
	}
	encoding := data[0]
	payload := data[1:]

	switch encoding {
	case 0: // ISO-8859-1
		return cleanString(string(payload))
	case 1: // UTF-16 con BOM
		if len(payload) >= 2 {
			bigEndian := (payload[0] == 0xFE && payload[1] == 0xFF)
			start := 0
			if (payload[0] == 0xFF && payload[1] == 0xFE) || bigEndian {
				start = 2
			}
			runes := make([]rune, 0, (len(payload)-start)/2)
			for i := start; i+1 < len(payload); i += 2 {
				var code uint16
				if bigEndian {
					code = binary.BigEndian.Uint16(payload[i : i+2])
				} else {
					code = binary.LittleEndian.Uint16(payload[i : i+2])
				}
				if code != 0 {
					runes = append(runes, rune(code))
				}
			}
			return cleanString(string(runes))
		}
	case 2: // UTF-16BE
		runes := make([]rune, 0, len(payload)/2)
		for i := 0; i+1 < len(payload); i += 2 {
			r := rune(binary.BigEndian.Uint16(payload[i : i+2]))
			if r != 0 {
				runes = append(runes, r)
			}
		}
		return cleanString(string(runes))
	case 3: // UTF-8
		return cleanString(string(payload))
	}
	return cleanString(string(payload))
}

// syncSafeInt convierte 4 bytes synchsafe a un entero normal de 28 bits (ID3v2).
func syncSafeInt(b []byte) int {
	if len(b) < 4 {
		return 0
	}
	return int(b[0]&0x7F)<<21 | int(b[1]&0x7F)<<14 | int(b[2]&0x7F)<<7 | int(b[3]&0x7F)
}

// cleanString elimina caracteres nulos o no imprimibles al inicio y final.
func cleanString(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return r == 0 || unicode.IsControl(r) || unicode.IsSpace(r)
	})
}

const defaultMaxEmbeddedCoverBytes = 2 << 20 // 2 MB máximo para carátulas embebidas

// detectEmbeddedImageExt deduce la extensión canónica de imagen verificando sus bytes mágicos.
func detectEmbeddedImageExt(data []byte) (string, bool) {
	if len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return ".jpg", true
	}
	if len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}) {
		return ".png", true
	}
	if len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")) {
		return ".webp", true
	}
	if len(data) >= 4 && bytes.Equal(data[:4], []byte("GIF8")) {
		return ".gif", true
	}
	if len(data) >= 2 && data[0] == 'B' && data[1] == 'M' {
		return ".bmp", true
	}
	return "", false
}

// skipEncodedNull avanza sobre una cadena terminada en 0x00 (ASCII/UTF-8) o 0x00 0x00 (UTF-16) y devuelve el índice posterior.
func skipEncodedNull(data []byte, start int, encoding byte) (int, bool) {
	if encoding == 1 || encoding == 2 {
		for i := start; i+1 < len(data); i += 2 {
			if data[i] == 0 && data[i+1] == 0 {
				return i + 2, true
			}
		}
		return len(data), false
	}
	idx := bytes.IndexByte(data[start:], 0)
	if idx < 0 {
		return len(data), false
	}
	return start + idx + 1, true
}

// parseID3PicturePayload extrae tipo de imagen y bytes binarios desde un frame ID3v2 APIC o PIC.
func parseID3PicturePayload(frameID string, body []byte) (byte, []byte, string) {
	if len(body) < 6 {
		return 0, nil, ""
	}
	encoding := body[0]
	pos := 1
	var picType byte

	if frameID == "PIC" {
		if len(body) < 6 {
			return 0, nil, ""
		}
		pos += 3 // Formato 3 bytes (JPG / PNG)
		picType = body[pos]
		pos++
	} else {
		nextPos, ok := skipEncodedNull(body, pos, 0)
		if !ok || nextPos >= len(body) {
			return 0, nil, ""
		}
		pos = nextPos
		picType = body[pos]
		pos++
	}

	if pos >= len(body) {
		return 0, nil, ""
	}
	imgOffset, ok := skipEncodedNull(body, pos, encoding)
	if !ok || imgOffset >= len(body) {
		return 0, nil, ""
	}
	imgBytes := body[imgOffset:]
	ext, valid := detectEmbeddedImageExt(imgBytes)
	if !valid {
		return 0, nil, ""
	}
	return picType, imgBytes, ext
}

// parseFLACPicturePayload decodifica un bloque METADATA_BLOCK_PICTURE de FLAC y valida sus bytes mágicos.
func parseFLACPicturePayload(block []byte, maxBytes int) (byte, []byte, string) {
	if len(block) < 32 {
		return 0, nil, ""
	}
	picType := byte(binary.BigEndian.Uint32(block[0:4]))
	pos := 4
	mimeLen := int(binary.BigEndian.Uint32(block[pos : pos+4]))
	pos += 4
	if mimeLen < 0 || pos+mimeLen+4 > len(block) {
		return 0, nil, ""
	}
	pos += mimeLen
	descLen := int(binary.BigEndian.Uint32(block[pos : pos+4]))
	pos += 4
	if descLen < 0 || pos+descLen+20 > len(block) {
		return 0, nil, ""
	}
	pos += descLen
	pos += 16 // width, height, color depth, indexed colors
	if pos+4 > len(block) {
		return 0, nil, ""
	}
	dataLen := int(binary.BigEndian.Uint32(block[pos : pos+4]))
	pos += 4
	if dataLen <= 0 || dataLen > maxBytes || pos+dataLen > len(block) {
		return 0, nil, ""
	}
	imgBytes := block[pos : pos+dataLen]
	ext, valid := detectEmbeddedImageExt(imgBytes)
	if !valid {
		return 0, nil, ""
	}
	return picType, imgBytes, ext
}

// extractFLACEmbeddedCover busca el bloque PICTURE (tipo 6) en un archivo FLAC acotado a maxBytes.
func extractFLACEmbeddedCover(r io.ReadSeeker, maxBytes int) ([]byte, string) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil || string(header) != "fLaC" {
		return nil, ""
	}

	var fallbackImg []byte
	var fallbackExt string
	blockHdr := make([]byte, 4)

	for {
		if _, err := io.ReadFull(r, blockHdr); err != nil {
			break
		}
		isLast := (blockHdr[0] & 0x80) != 0
		blockType := blockHdr[0] & 0x7F
		length := int(blockHdr[1])<<16 | int(blockHdr[2])<<8 | int(blockHdr[3])

		if blockType == 6 && length > 32 && length <= maxBytes+4096 {
			blockData := make([]byte, length)
			if _, err := io.ReadFull(r, blockData); err != nil {
				break
			}
			picType, img, ext := parseFLACPicturePayload(blockData, maxBytes)
			if len(img) > 0 {
				if picType == 3 {
					return img, ext
				}
				if len(fallbackImg) == 0 {
					fallbackImg = img
					fallbackExt = ext
				}
			}
		} else {
			if _, err := r.Seek(int64(length), io.SeekCurrent); err != nil {
				break
			}
		}
		if isLast {
			break
		}
	}
	return fallbackImg, fallbackExt
}

// extractID3EmbeddedCover busca frames APIC/PIC en cabeceras ID3v2 respetando un límite máximo de lectura.
func extractID3EmbeddedCover(r io.ReadSeeker, maxBytes int) ([]byte, string) {
	header := make([]byte, 10)
	if _, err := io.ReadFull(r, header); err != nil || string(header[:3]) != "ID3" {
		return nil, ""
	}
	version := header[3]
	flags := header[5]
	tagSize := syncSafeInt(header[6:10])
	if version < 2 || version > 4 || tagSize <= 0 || tagSize > 20*1024*1024 {
		return nil, ""
	}

	extSkip := 0
	if (version == 3 || version == 4) && (flags&0x40) != 0 && tagSize > 4 {
		extHdr := make([]byte, 4)
		if _, err := io.ReadFull(r, extHdr); err == nil {
			if version == 4 {
				extSize := syncSafeInt(extHdr)
				if extSize > 4 && extSize <= tagSize {
					_, _ = r.Seek(int64(extSize-4), io.SeekCurrent)
					extSkip = extSize
				}
			} else {
				extSize := int(binary.BigEndian.Uint32(extHdr))
				if extSize >= 0 && extSize+4 <= tagSize {
					_, _ = r.Seek(int64(extSize), io.SeekCurrent)
					extSkip = extSize + 4
				}
			}
		}
	}

	remaining := tagSize - extSkip
	hdrLen := 10
	if version == 2 {
		hdrLen = 6
	}
	frameHdr := make([]byte, hdrLen)
	pos := 0
	var fallbackImg []byte
	var fallbackExt string

	for pos+hdrLen <= remaining {
		if _, err := io.ReadFull(r, frameHdr); err != nil {
			break
		}
		pos += hdrLen
		if frameHdr[0] == 0 {
			break
		}
		frameID, frameSize := parseID3FrameHeader(frameHdr, version)
		if !isValidFrameID(frameID) || frameSize <= 0 || int64(pos)+int64(frameSize) > int64(remaining) {
			break
		}
		if (frameID == "APIC" || frameID == "PIC") && frameSize <= maxBytes+4096 {
			body := make([]byte, frameSize)
			if _, err := io.ReadFull(r, body); err != nil {
				break
			}
			picType, img, ext := parseID3PicturePayload(frameID, body)
			if len(img) > 0 && len(img) <= maxBytes {
				if picType == 3 {
					return img, ext
				}
				if len(fallbackImg) == 0 {
					fallbackImg = img
					fallbackExt = ext
				}
			}
		} else {
			if _, err := r.Seek(int64(frameSize), io.SeekCurrent); err != nil {
				break
			}
		}
		pos += frameSize
	}
	return fallbackImg, fallbackExt
}

// ExtractEmbeddedCover extrae una carátula embebida (ID3v2 APIC o FLAC PICTURE) acotada en bytes y validada por bytes mágicos.
func ExtractEmbeddedCover(filePath string, maxBytes int) ([]byte, string) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxEmbeddedCoverBytes
	}
	f, err := os.Open(filePath)
	if err != nil {
		return nil, ""
	}
	defer f.Close()

	ext := strings.ToLower(filePath)
	if strings.HasSuffix(ext, ".flac") {
		return extractFLACEmbeddedCover(f, maxBytes)
	}
	if strings.HasSuffix(ext, ".mp3") {
		return extractID3EmbeddedCover(f, maxBytes)
	}
	return nil, ""
}
