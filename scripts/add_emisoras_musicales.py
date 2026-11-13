#!/usr/bin/env python3
"""
Script para agregar el catálogo oficial de Emisoras Musicales y Motiva FM
en los ficheros default_radios.json y en la base de datos activa SQLite (aurabox.db).
"""

import json
import os
import sqlite3

NEW_STATIONS = [
    # 1. La Indie
    {
        "id": 2001,
        "name": "La Indie",
        "url": "https://stream.emisorasmusicales.net/listen/la_indie/laindie.mp3",
        "logo_url": "https://media.emisorasmusicales.net/wp-content/uploads/2020/04/04135806/LaIndie-5.svg",
        "genre": "Indie, Alternative, Pop-Rock",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },
    {
        "id": 2002,
        "name": "La Indie - Madrid",
        "url": "https://stream.emisorasmusicales.net/listen/la_indie/laindie-madrid.mp3",
        "logo_url": "https://media.emisorasmusicales.net/wp-content/uploads/2020/04/04135806/LaIndie-5.svg",
        "genre": "Indie, Alternative, Pop-Rock",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },
    {
        "id": 2003,
        "name": "La Indie - Valencia",
        "url": "https://stream.emisorasmusicales.net/listen/la_indie/laindie-valencia.mp3",
        "logo_url": "https://media.emisorasmusicales.net/wp-content/uploads/2020/04/04135806/LaIndie-5.svg",
        "genre": "Indie, Alternative, Pop-Rock",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },

    # 2. Activa FM
    {
        "id": 2004,
        "name": "Activa FM",
        "url": "https://stream.emisorasmusicales.net/listen/activa_fm/activafm.mp3",
        "logo_url": "https://www.emisorasmusicales.net/wp-content/uploads/2020/05/activa.svg",
        "genre": "Dance, Pop, Top 40",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },
    {
        "id": 2005,
        "name": "Activa FM - Valencia",
        "url": "https://stream.emisorasmusicales.net/listen/activa_fm/activafm-valencia.mp3",
        "logo_url": "https://www.emisorasmusicales.net/wp-content/uploads/2020/05/activa.svg",
        "genre": "Dance, Pop, Top 40",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },
    {
        "id": 2006,
        "name": "Activa FM - Alicante",
        "url": "https://stream.emisorasmusicales.net/listen/activa_fm/activafm-alicante.mp3",
        "logo_url": "https://www.emisorasmusicales.net/wp-content/uploads/2020/05/activa.svg",
        "genre": "Dance, Pop, Top 40",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },
    {
        "id": 2007,
        "name": "Activa FM - Benidorm",
        "url": "https://stream.emisorasmusicales.net/listen/activa_fm/activafm-marina-baja-benidorm.mp3",
        "logo_url": "https://www.emisorasmusicales.net/wp-content/uploads/2020/05/activa.svg",
        "genre": "Dance, Pop, Top 40",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },
    {
        "id": 2008,
        "name": "Activa FM - Marina Alta",
        "url": "https://stream.emisorasmusicales.net/listen/activa_fm/activafm-marina-alta-norte.mp3",
        "logo_url": "https://www.emisorasmusicales.net/wp-content/uploads/2020/05/activa.svg",
        "genre": "Dance, Pop, Top 40",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },

    # 3. La Mega
    {
        "id": 2009,
        "name": "La Mega",
        "url": "https://stream.emisorasmusicales.net/listen/la_mega/lamega.mp3",
        "logo_url": "https://media.emisorasmusicales.net/wp-content/uploads/2020/03/23102600/la-mega.svg",
        "genre": "EDM, Dance, Electro",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },
    {
        "id": 2010,
        "name": "La Mega - Valencia",
        "url": "https://stream.emisorasmusicales.net/listen/la_mega/lamega-valencia.mp3",
        "logo_url": "https://media.emisorasmusicales.net/wp-content/uploads/2020/03/23102600/la-mega.svg",
        "genre": "EDM, Dance, Electro",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },
    {
        "id": 2011,
        "name": "La Mega - Alicante",
        "url": "https://stream.emisorasmusicales.net/listen/la_mega/lamega-levante.mp3",
        "logo_url": "https://media.emisorasmusicales.net/wp-content/uploads/2020/03/23102600/la-mega.svg",
        "genre": "EDM, Dance, Electro",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },

    # 4. La Flamenca
    {
        "id": 2012,
        "name": "La Flamenca",
        "url": "https://stream.emisorasmusicales.net/listen/la_flamenca/laflamenca.mp3",
        "logo_url": "https://media.emisorasmusicales.net/wp-content/uploads/2020/03/23102601/la-flamenca-.svg",
        "genre": "Flamenco, Copla, Español",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },
    {
        "id": 2013,
        "name": "La Flamenca - Valencia",
        "url": "https://stream.emisorasmusicales.net/listen/la_flamenca/laflamenca-valencia.mp3",
        "logo_url": "https://media.emisorasmusicales.net/wp-content/uploads/2020/03/23102601/la-flamenca-.svg",
        "genre": "Flamenco, Copla, Español",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },

    # 5. Muy Buena
    {
        "id": 2014,
        "name": "Muy Buena",
        "url": "https://stream.emisorasmusicales.net/listen/muybuena/muybuena.mp3",
        "logo_url": "https://media.emisorasmusicales.net/wp-content/uploads/2020/03/27120547/NUEVO-LOGO-MB-2.png",
        "genre": "Latino, Reggaeton, Pop Urbano",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },
    {
        "id": 2015,
        "name": "Muy Buena - Valencia",
        "url": "https://stream.emisorasmusicales.net/listen/muybuena/muybuena-valencia.mp3",
        "logo_url": "https://media.emisorasmusicales.net/wp-content/uploads/2020/03/27120547/NUEVO-LOGO-MB-2.png",
        "genre": "Latino, Reggaeton, Pop Urbano",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },

    # 6. Motiva
    {
        "id": 2016,
        "name": "Motiva",
        "url": "https://stream.guadigo.es/listen/motiva/motiva.mp3",
        "logo_url": "https://motivafm.com/wp-content/uploads/2021/06/motiva.svg",
        "genre": "Reggaeton, Urbano, Perreo",
        "broadcaster": "Motiva FM",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },
    {
        "id": 2017,
        "name": "Motiva - Valencia",
        "url": "https://stream.guadigo.es/listen/motiva/motiva-valencia",
        "logo_url": "https://motivafm.com/wp-content/uploads/2021/06/motiva.svg",
        "genre": "Reggaeton, Urbano, Perreo",
        "broadcaster": "Motiva FM",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },

    # 7. Bikini FM
    {
        "id": 2018,
        "name": "Bikini FM",
        "url": "https://stream.emisorasmusicales.net/listen/bikini_fm/bikinifm.mp3",
        "logo_url": "https://stream.emisorasmusicales.net/static/uploads/bikini_fm/album_art.1782121996.jpg",
        "genre": "Dance 90s, Eurodance, Remember",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },
    {
        "id": 2019,
        "name": "Bikini FM - Valencia",
        "url": "https://stream.emisorasmusicales.net/listen/bikini_fm/bikinifm-valencia.mp3",
        "logo_url": "https://stream.emisorasmusicales.net/static/uploads/bikini_fm/album_art.1782121996.jpg",
        "genre": "Dance 90s, Eurodance, Remember",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },

    # 8. Esencia FM
    {
        "id": 2020,
        "name": "Esencia FM",
        "url": "https://stream.emisorasmusicales.net/listen/esencia_fm/esenciafm.mp3",
        "logo_url": "https://media.emisorasmusicales.net/wp-content/uploads/2020/03/23102603/esenciafm.svg",
        "genre": "Pop Español, Baladas",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },

    # 9. Comboi FM
    {
        "id": 2021,
        "name": "Comboi FM",
        "url": "https://stream.emisorasmusicales.net/listen/comboi/comboi.mp3",
        "logo_url": "https://media.emisorasmusicales.net/wp-content/uploads/2024/04/29155408/LOGO-DEF.-COMBOI-scaled.jpg",
        "genre": "Música en Valencià, Festiva, Pop-Rock",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    },

    # 10. Gold FM
    {
        "id": 2022,
        "name": "Gold FM",
        "url": "https://stream.emisorasmusicales.net/listen/gold_fm/goldfm.mp3",
        "logo_url": "https://media.emisorasmusicales.net/wp-content/uploads/2020/03/17111707/gold-1000x1000_Mesa-de-trabajo-1.png",
        "genre": "Oldies, 70s, 80s, Pop Clásico",
        "broadcaster": "Emisoras Musicales",
        "country": "Spain",
        "bitrate": "128",
        "format": "MP3"
    }
]


def update_json_catalog(file_path):
    """Actualiza o incorpora las emisoras en un fichero JSON preservando el orden alfabético."""
    if not os.path.exists(file_path):
        print(f"[-] Archivo no encontrado: {file_path}")
        return

    with open(file_path, "r", encoding="utf-8") as f:
        existing = json.load(f)

    existing_urls = {item.get("url") or item.get("stream_url") for item in existing}
    added_count = 0

    for st in NEW_STATIONS:
        if st["url"] not in existing_urls:
            existing.append(st)
            existing_urls.add(st["url"])
            added_count += 1
        else:
            # Actualizar metadatos si ya existía
            for idx, old in enumerate(existing):
                if (old.get("url") or old.get("stream_url")) == st["url"]:
                    existing[idx].update(st)
                    break

    # Ordenar por nombre
    existing.sort(key=lambda x: x.get("name", "").lower())

    with open(file_path, "w", encoding="utf-8") as f:
        json.dump(existing, f, indent=2, ensure_ascii=False)

    print(f"[+] {file_path}: {added_count} emisoras añadidas/actualizadas (Total: {len(existing)})")


def update_sqlite_db(db_path):
    """Inserta o actualiza las emisoras en la tabla radio_stations de SQLite."""
    if not os.path.exists(db_path):
        print(f"[-] Base de datos no encontrada: {db_path}")
        return

    conn = sqlite3.connect(db_path)
    cur = conn.cursor()

    query = """
        INSERT INTO radio_stations (name, stream_url, logo_url, genre, broadcaster, country, bitrate, format, is_default)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)
        ON CONFLICT(stream_url) DO UPDATE SET
            name=excluded.name,
            logo_url=CASE WHEN excluded.logo_url != '' THEN excluded.logo_url ELSE radio_stations.logo_url END,
            genre=excluded.genre,
            broadcaster=excluded.broadcaster,
            country=excluded.country,
            bitrate=excluded.bitrate,
            format=excluded.format
    """

    count = 0
    for st in NEW_STATIONS:
        cur.execute(query, (
            st["name"],
            st["url"],
            st["logo_url"],
            st["genre"],
            st["broadcaster"],
            st["country"],
            st["bitrate"],
            st["format"]
        ))
        count += 1

    conn.commit()
    conn.close()
    print(f"[+] SQLite {db_path}: {count} emisoras procesadas y guardadas.")


if __name__ == "__main__":
    update_json_catalog("internal/db/default_radios.json")
    update_json_catalog("data/default_radios.json")
    update_sqlite_db("data/aurabox.db")
