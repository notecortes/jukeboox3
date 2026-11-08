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

	for {
		if _, err := io.ReadFull(r, blockHeader); err != nil {
			break
		}
		isLast := (blockHeader[0] & 0x80) != 0
		blockType := blockHeader[0] & 0x7F
		length := int(blockHeader[1])<<16 | int(blockHeader[2])<<8 | int(blockHeader[3])

		// Bloque 0: STREAMINFO (duración exacta, sample rate, canales, bits)
		if blockType == 0 && length >= 34 {
			infoData := make([]byte, length)
			if _, err := io.ReadFull(r, infoData); err != nil {
				break
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
		} else if blockType == 4 { // Bloque 4: Vorbis Comment
			blockData := make([]byte, length)
			if _, err := io.ReadFull(r, blockData); err != nil {
				break
			}
			parseVorbisComments(blockData, meta)
		} else {
			// Saltar otros bloques
			if _, err := io.CopyN(io.Discard, r, int64(length)); err != nil {
				break
			}
		}

		if isLast {
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
	if err := binary.Read(reader, binary.LittleEndian, &vendorLen); err != nil || int(vendorLen) > len(data) {
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
		tagSize := syncSafeInt(header[6:10])
		if tagSize > 0 && tagSize <= 10*1024*1024 {
			audioStartOffset = int64(10 + tagSize)
			tagData := make([]byte, tagSize)
			if _, err := io.ReadFull(r, tagData); err == nil {
				offset := 0
				for offset < len(tagData)-10 {
					var frameID string
					var frameSize int

					if version == 2 {
						if offset+6 > len(tagData) {
							break
						}
						frameID = string(tagData[offset : offset+3])
						frameSize = int(tagData[offset+3])<<16 | int(tagData[offset+4])<<8 | int(tagData[offset+5])
						offset += 6
					} else {
						if offset+10 > len(tagData) {
							break
						}
						frameID = string(tagData[offset : offset+4])
						if version == 4 {
							frameSize = syncSafeInt(tagData[offset+4 : offset+8])
						} else {
							frameSize = int(binary.BigEndian.Uint32(tagData[offset+4 : offset+8]))
						}
						offset += 10
					}

					if frameSize <= 0 || offset+frameSize > len(tagData) || frameID[0] == 0 {
						break
					}

					frameBody := tagData[offset : offset+frameSize]
					offset += frameSize

					val := decodeTextFrame(frameBody)
					if val == "" {
						continue
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
			runes := make([]rune, 0, len(payload)/2)
			for i := 2; i+1 < len(payload); i += 2 {
				r := rune(binary.LittleEndian.Uint16(payload[i : i+2]))
				if r != 0 {
					runes = append(runes, r)
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
