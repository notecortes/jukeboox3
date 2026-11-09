package scanner

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// encodeSynchsafe convierte un entero de 28 bits al formato synchsafe de ID3v2.
func encodeSynchsafe(v int) [4]byte {
	return [4]byte{
		byte((v >> 21) & 0x7F),
		byte((v >> 14) & 0x7F),
		byte((v >> 7) & 0x7F),
		byte(v & 0x7F),
	}
}

// buildID3v23Frame construye un frame ID3v2.3 binario con tamaño estándar de 32 bits big-endian.
func buildID3v23Frame(id string, body []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString(id)
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(body)))
	buf.Write([]byte{0x00, 0x00}) // flags
	buf.Write(body)
	return buf.Bytes()
}

// buildUTF16LEFrameBody codifica texto con encoding=1 y BOM UTF-16LE para probar decodificación de acentos.
func buildUTF16LEFrameBody(text string) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x01)       // UTF-16 con BOM
	buf.Write([]byte{0xFF, 0xFE}) // BOM little-endian
	for _, r := range text {
		_ = binary.Write(&buf, binary.LittleEndian, uint16(r))
	}
	return buf.Bytes()
}

// TestReadID3v2SkipsLargeAPICAndDecodesUTF16 verifica el salto por Seek de carátulas pesadas APIC y lectura UTF-16LE.
func TestReadID3v2SkipsLargeAPICAndDecodesUTF16(t *testing.T) {
	var frames bytes.Buffer
	frames.Write(buildID3v23Frame("TIT2", buildUTF16LEFrameBody("Canción Ágil")))
	// Bloque APIC artificial de 90 KB para superar el umbral de 64 KB y activar streamID3v2Frames
	apic := make([]byte, 90*1024)
	copy(apic, "image/jpeg\x00\x03cover\x00")
	frames.Write(buildID3v23Frame("APIC", apic))
	frames.Write(buildID3v23Frame("TPE1", append([]byte{0x03}, []byte("Héroes del Silencio")...)))
	frames.Write(buildID3v23Frame("TALB", append([]byte{0x03}, []byte("Senderos de Traición")...)))

	tagBytes := frames.Bytes()
	ss := encodeSynchsafe(len(tagBytes))

	var fileBuf bytes.Buffer
	fileBuf.WriteString("ID3")
	fileBuf.WriteByte(0x03) // ID3v2.3
	fileBuf.WriteByte(0x00)
	fileBuf.WriteByte(0x00)
	fileBuf.Write(ss[:])
	fileBuf.Write(tagBytes)

	// Añadir cabecera de frame MPEG-1 Layer III CBR 128kbps 44100Hz + 32000 bytes de payload (~2s)
	fileBuf.Write([]byte{0xFF, 0xFB, 0x90, 0x00})
	fileBuf.Write(make([]byte, 32000))

	mp3Path := filepath.Join(t.TempDir(), "test.mp3")
	if err := os.WriteFile(mp3Path, fileBuf.Bytes(), 0644); err != nil {
		t.Fatalf("WriteFile falló: %v", err)
	}

	meta := ReadAudioTags(mp3Path)
	if meta == nil {
		t.Fatalf("ReadAudioTags devolvió nil")
	}
	if meta.Title != "Canción Ágil" {
		t.Fatalf("título inesperado: %q", meta.Title)
	}
	if meta.Artist != "Héroes del Silencio" {
		t.Fatalf("artista inesperado: %q", meta.Artist)
	}
	if meta.Album != "Senderos de Traición" {
		t.Fatalf("álbum inesperado: %q", meta.Album)
	}
	if meta.Bitrate != 128 || meta.SampleRate != 44100 {
		t.Fatalf("propiedades MPEG inesperadas: bitrate=%d sr=%d", meta.Bitrate, meta.SampleRate)
	}
	if meta.Duration <= 0 {
		t.Fatalf("duración esperada > 0, recibida %v", meta.Duration)
	}
}

// buildVorbisCommentBlock construye un bloque FLAC VORBIS_COMMENT válido en formato little-endian.
func buildVorbisCommentBlock(comments []string) []byte {
	var buf bytes.Buffer
	vendor := []byte("AuraBoxTest")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(vendor)))
	buf.Write(vendor)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(comments)))
	for _, c := range comments {
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(c)))
		buf.WriteString(c)
	}
	return buf.Bytes()
}

// TestReadFLACSkipsPictureBlockAndReadsVorbis comprueba lectura STREAMINFO y Vorbis Comments saltando PICTURE por Seek.
func TestReadFLACSkipsPictureBlockAndReadsVorbis(t *testing.T) {
	var flac bytes.Buffer
	flac.WriteString("fLaC")

	// Bloque 0: STREAMINFO (34 bytes): 44100 Hz y 441000 muestras = 10.0s exactos
	streamInfo := make([]byte, 34)
	sr := uint32(44100)
	samples := uint64(441000)
	streamInfo[10] = byte(sr >> 12)
	streamInfo[11] = byte(sr >> 4)
	streamInfo[12] = byte((sr&0x0F)<<4) | 0x02 // 2 canales-1 = 1
	streamInfo[13] = byte((samples >> 32) & 0x0F)
	streamInfo[14] = byte(samples >> 24)
	streamInfo[15] = byte(samples >> 16)
	streamInfo[16] = byte(samples >> 8)
	streamInfo[17] = byte(samples)

	flac.Write([]byte{0x00, 0x00, 0x00, 34})
	flac.Write(streamInfo)

	// Bloque 6: PICTURE de 80 KB (no es el último)
	picLen := 80 * 1024
	flac.Write([]byte{0x06, byte(picLen >> 16), byte(picLen >> 8), byte(picLen)})
	flac.Write(make([]byte, picLen))

	// Bloque 4: VORBIS_COMMENT marcado como último bloque (0x80 | 4 = 0x84)
	vorbis := buildVorbisCommentBlock([]string{
		"TITLE=Autoterapia",
		"ARTIST=IZAL",
		"ALBUM=Autoterapia",
		"DATE=2018-03-09",
		"TRACKNUMBER=2/11",
	})
	vLen := len(vorbis)
	flac.Write([]byte{0x84, byte(vLen >> 16), byte(vLen >> 8), byte(vLen)})
	flac.Write(vorbis)

	flacPath := filepath.Join(t.TempDir(), "test.flac")
	if err := os.WriteFile(flacPath, flac.Bytes(), 0644); err != nil {
		t.Fatalf("WriteFile falló: %v", err)
	}

	meta := ReadAudioTags(flacPath)
	if meta == nil {
		t.Fatalf("ReadAudioTags devolvió nil")
	}
	if meta.Title != "Autoterapia" || meta.Artist != "IZAL" || meta.Album != "Autoterapia" {
		t.Fatalf("metadatos Vorbis inesperados: %+v", meta)
	}
	if meta.Year != 2018 || meta.TrackNumber != 2 {
		t.Fatalf("año o pista inesperados: year=%d track=%d", meta.Year, meta.TrackNumber)
	}
	if meta.Duration != 10.0 || meta.SampleRate != 44100 {
		t.Fatalf("propiedades STREAMINFO inesperadas: dur=%v sr=%d", meta.Duration, meta.SampleRate)
	}
}

// TestParseTrackMetadataFolderHeuristics valida la inferencia de carpetas con categorías y álbumes faltantes.
func TestParseTrackMetadataFolderHeuristics(t *testing.T) {
	root := t.TempDir()
	coversDir := filepath.Join(root, "covers")
	ls := NewLibraryScanner(nil, nil, root, root, root, coversDir)

	// Caso 1: carpeta de categoría + artista sin subcarpeta de álbum
	singlePath := filepath.Join(root, "01.clasica", "ludovico", "01 - Nuvole Bianche.mp3")
	if err := os.MkdirAll(filepath.Dir(singlePath), 0755); err != nil {
		t.Fatalf("MkdirAll falló: %v", err)
	}
	if err := os.WriteFile(singlePath, []byte("dummy"), 0644); err != nil {
		t.Fatalf("WriteFile falló: %v", err)
	}
	info1, _ := os.Stat(singlePath)
	t1 := ls.parseTrackMetadata(singlePath, root, "nas", info1)
	if t1.Artist != "Ludovico Einaudi" || t1.Album != "Sencillos" || t1.Genre != "Clásica" {
		t.Fatalf("heurística categoría/sencillo incorrecta: %+v", t1)
	}

	// Caso 2: carpeta de categoría + artista + álbum con año y etiquetas técnicas
	albPath := filepath.Join(root, "04.indie", "izal", "2018 - Autoterapia [FLAC]", "01 - Autoterapia.flac")
	if err := os.MkdirAll(filepath.Dir(albPath), 0755); err != nil {
		t.Fatalf("MkdirAll falló: %v", err)
	}
	if err := os.WriteFile(albPath, []byte("dummy"), 0644); err != nil {
		t.Fatalf("WriteFile falló: %v", err)
	}
	info2, _ := os.Stat(albPath)
	t2 := ls.parseTrackMetadata(albPath, root, "nas", info2)
	if t2.Artist != "IZAL" || t2.Album != "Autoterapia" || t2.Year != 2018 {
		t.Fatalf("heurística álbum/año incorrecta: %+v", t2)
	}
}

