package podcasts

import (
	"strings"
	"testing"
)

// TestParseDurationSec valida la conversión de marcas de tiempo en segundos.
func TestParseDurationSec(t *testing.T) {
	cases := []struct {
		input    string
		expected float64
	}{
		{"01:23:45", 5025},
		{"43:27", 2607},
		{"2450", 2450},
		{"00:05", 5},
		{"", 0},
		{"invalid", 0},
	}

	for _, tc := range cases {
		got := parseDurationSec(tc.input)
		if got != tc.expected {
			t.Errorf("parseDurationSec(%q) = %v, esperado %v", tc.input, got, tc.expected)
		}
	}
}

// TestParseFeedXML valida la extracción de canales y episodios desde un documento RSS válido.
func TestParseFeedXML(t *testing.T) {
	xmlSample := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
  <channel>
    <title><![CDATA[Ciencia y Sonido]]></title>
    <description>Un programa dedicado a la divulgación científica.</description>
    <link>https://ejemplo.com/podcast</link>
    <itunes:author>Dra. Elena Ruiz</itunes:author>
    <itunes:image href="https://ejemplo.com/cover.jpg"/>
    <item>
      <title>Episodio 1: Acústica Cuántica</title>
      <description>&lt;p&gt;Explorando ondas sonoras.&lt;/p&gt;</description>
      <guid>ep-001</guid>
      <pubDate>Mon, 02 Nov 2026 10:00:00 GMT</pubDate>
      <enclosure url="https://ejemplo.com/audio/ep1.mp3" length="1234567" type="audio/mpeg"/>
      <itunes:duration>45:20</itunes:duration>
    </item>
    <item>
      <title>Texto sin audio</title>
      <description>Este item no tiene enclosure.</description>
    </item>
  </channel>
</rss>`)

	feedURL := "https://ejemplo.com/rss.xml"
	pod, episodes, err := ParseFeedXML(xmlSample, feedURL)
	if err != nil {
		t.Fatalf("ParseFeedXML falló inesperadamente: %v", err)
	}

	if pod.Title != "Ciencia y Sonido" {
		t.Errorf("título inesperado: %q", pod.Title)
	}
	if pod.Author != "Dra. Elena Ruiz" {
		t.Errorf("autor inesperado: %q", pod.Author)
	}
	if pod.CoverURL != "https://ejemplo.com/cover.jpg" {
		t.Errorf("cover inesperado: %q", pod.CoverURL)
	}
	if len(episodes) != 1 {
		t.Fatalf("esperado 1 episodio con enclosure, obtenido %d", len(episodes))
	}

	ep := episodes[0]
	if ep.GUID != "ep-001" {
		t.Errorf("guid inesperado: %q", ep.GUID)
	}
	if ep.Title != "Episodio 1: Acústica Cuántica" {
		t.Errorf("título de episodio inesperado: %q", ep.Title)
	}
	if ep.EnclosureURL != "https://ejemplo.com/audio/ep1.mp3" {
		t.Errorf("URL enclosure inesperada: %q", ep.EnclosureURL)
	}
	if ep.Duration != 2720 { // 45 * 60 + 20
		t.Errorf("duración inesperada: %v", ep.Duration)
	}
	if ep.Description != "Explorando ondas sonoras." {
		t.Errorf("descripción no limpiada: %q", ep.Description)
	}
}

// TestParseAndGenerateOPML verifica la lectura recursiva de OPML y la regeneración fiel en XML.
func TestParseAndGenerateOPML(t *testing.T) {
	opmlSample := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <head>
    <title>Mis Podcasts</title>
  </head>
  <body>
    <outline text="Tecnología">
      <outline text="Tech News" title="Tech News" type="rss" xmlUrl="https://tech.example.com/rss" htmlUrl="https://tech.example.com"/>
    </outline>
    <outline text="Música" type="rss" xmlUrl="https://music.example.com/rss" htmlUrl="https://music.example.com"/>
  </body>
</opml>`)

	items, err := ParseOPML(opmlSample)
	if err != nil {
		t.Fatalf("ParseOPML falló: %v", err)
	}

	if len(items) != 2 {
		t.Fatalf("esperados 2 feeds descubiertos, obtenidos %d", len(items))
	}

	if items[0].XMLURL != "https://tech.example.com/rss" {
		t.Errorf("feed 0 inesperado: %q", items[0].XMLURL)
	}
	if items[1].XMLURL != "https://music.example.com/rss" {
		t.Errorf("feed 1 inesperado: %q", items[1].XMLURL)
	}

	// Probar generación OPML
	podcastsList := []Podcast{
		{Title: "Podcast A", FeedURL: "https://a.com/rss", Link: "https://a.com"},
		{Title: "Podcast B", FeedURL: "https://b.com/rss", Link: "https://b.com"},
	}

	generated, err := GenerateOPML(podcastsList)
	if err != nil {
		t.Fatalf("GenerateOPML falló: %v", err)
	}

	if !strings.Contains(string(generated), "<opml version=\"2.0\">") {
		t.Errorf("cabecera OPML faltante en: %s", string(generated))
	}

	// Re-parsear el OPML generado
	reparsed, err := ParseOPML(generated)
	if err != nil {
		t.Fatalf("error re-parseando OPML generado: %v", err)
	}
	if len(reparsed) != 2 {
		t.Fatalf("esperados 2 feeds en re-parseo, obtenidos %d", len(reparsed))
	}
}
