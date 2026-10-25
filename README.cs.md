# ssmon – monitor posluchačů SpyServeru

[English](README.md) | **Čeština**

**ssmon** zaznamenává, kdo, odkud, kdy, jak dlouho a v jakém pásmu poslouchá na veřejném
[Airspy SpyServeru](https://airspy.com/). Vše zobrazuje na webovém dashboardu s mapou a statistikami.

Nic nevkládá do datové cesty SpyServeru. Jen pasivně čte stavové reporty, které SpyServer každých
~15 s posílá do veřejného adresáře Airspy. Když ssmon spadne, SpyServer i posluchači běží dál.

- jeden statický binární soubor v Go (bez CGO, bez runtime závislostí, Leaflet je zabudovaný),
- data v SQLite, statistiky se počítají dotazy SQL na serveru (rychlé i s roky historie),
- offline geolokace IP adres (DB-IP Lite / GeoLite2, soubory MMDB), bez externích API,
- filtr BPF v jádře propustí do programu jen pakety reportů, IQ stream na stejném rozhraní nic nestojí,
- běží jako neprivilegovaný uživatel s jediným oprávněním `CAP_NET_RAW`, spotřeba RAM ~10–20 MB,
- dvojjazyčný dashboard (čeština / angličtina), přepínání vlajkami.

---

## Obsah
1. [Jak to funguje](#jak-to-funguje)
2. [Omezení](#omezení)
3. [Požadavky](#požadavky)
4. [Sestavení](#sestavení)
5. [Ruční test](#ruční-test)
6. [Instalace pro ostrý provoz](#instalace-pro-ostrý-provoz)
7. [Popis souboru ssmon.service](#popis-souboru-ssmonservice)
8. [Parametry](#parametry)
9. [GeoIP databáze a jejich aktualizace](#geoip-databáze-a-jejich-aktualizace)
10. [Pásma (bands.txt)](#pásma-bandstxt)
11. [Dashboard](#dashboard)
12. [Databáze](#databáze)
13. [Údržba a aktualizace](#údržba-a-aktualizace)
14. [Řešení problémů](#řešení-problémů)
15. [Struktura projektu](#struktura-projektu)
16. [Licence a uvedení zdrojů](#licence-a-uvedení-zdrojů)

---

## Jak to funguje

```
posluchač ──► VPS (veřejná IP) ──WireGuard──► RPi + SpyServer
                    │                              │
                    │◄──── POST /register ─────────┘  (každých ~15 s)
                    │      directory.api.airspy.com:8080
                  ssmon (pasivně čte reporty na wg0)
                    │
                  SQLite ──► dashboard :8090
```

SpyServer s `list_in_directory = 1` posílá do adresáře Airspy prostý HTTP report
(`POST /register`, `Content-Type: application/x-sdr-server-status`) ve formátu `Klíč=Hodnota`:

```
ServerVersion=2.0.1921
MaxClients=1
CurrentClientCount=1
CurrentClients=203.0.113.45
CurrentCenterFrequency=145387999
AntennaLocation=50.0755, 14.4378
...
```

ssmon tyto pakety zachytí přes `AF_PACKET` socket na zvoleném rozhraní (typicky WireGuard tunel
mezi VPS a RPi) a skládá z nich **relace**.

### Ukládají se jen změny, ne každý report

| událost v reportu | zápis do databáze |
|---|---|
| objeví se nová IP | **nový řádek** v `sessions` (IP, začátek, geolokace) a první řádek v `tunings` |
| stejná IP, stejný kmitočet | jen **UPDATE** `last_seen` a počítadla reportů (soubor neroste) |
| stejná IP, jiný kmitočet | **nový řádek** v `tunings` (čas a kmitočet) |
| IP zmizí nebo 45 s nepřijde report | relace se uzavře (`open = 0`) |
| report bez klienta | přepíše se jediný řádek se stavem serveru (online/offline) |

Příklad: posluchač je připojený 3 hodiny a čtyřikrát přeladí. Za tu dobu přijde 720 reportů,
v databázi z nich zůstane **1 řádek relace a 5 řádků ladění**. Délka relace se počítá jako
`last_seen − začátek` a čas na jednotlivých kmitočtech z rozdílů časů ladění.

---

## Omezení

- **Kmitočet je středový kmitočet přijímače** (`CurrentCenterFrequency`), ne VFO posluchače. Posluchač
  může poslouchat kdekoli v zobrazené šířce pásma kolem středu. Kmitočty se proto sčítají
  a zobrazují jen **na celé MHz** (145,000–145,999 = „145 MHz“). Pásmo se ale vždy určuje z přesné
  hodnoty.
- **Výchozí kmitočet:** SpyServer po připojení naladí klienta na `initial_frequency`. S parametrem
  `-initial-mhz` se první naladění relace na tento kmitočet do statistik nepočítá. V průběhu ladění
  zůstane vidět s poznámkou „výchozí kmitočet serveru (nezapočítáno)“.
- **Rozlišení času je ~15 s** (interval reportů). Relace kratší než jeden interval nemusí být vidět
  vůbec a délky jsou přibližné.
- Počítá se s **`MaxClients=1`**, tedy s jednou IP v `CurrentClients`.
- Verze klientského SW se v reportech nepřenáší.
- Jen IPv4 (reporty i klienti).

---

## Požadavky

**Server (kde ssmon běží):**
- Linux se systemd (testováno na Ubuntu), architektura amd64 nebo arm64,
- rozhraní, kterým tečou reporty SpyServeru do adresáře (např. `wg0` na VPS),
- `curl` a `gunzip` pro stahování GeoIP databází,
- volitelně `sqlite3` pro vlastní dotazy a zálohy.

**Sestavení:** Go ≥ 1.22 na libovolném počítači (Windows, Linux, macOS). Na slabé VPS s 1 GB RAM
sestavení nedoporučuji, protože kompilace ovladače SQLite potřebuje hodně paměti. Sestav jinde
a binárku nakopíruj.

---

## Sestavení

Při prvním sestavení stáhne `go mod tidy` závislosti a vytvoří `go.sum`. Ten patří do repozitáře.

### Windows (PowerShell)

Go nainstaluješ instalátorem `.msi` z <https://go.dev/dl/> (nebo `winget install GoLang.Go`).
Pak **otevři nové okno** PowerShellu.

```powershell
cd C:\cesta\k\ssmon            # folder containing go.mod
go mod tidy
$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
go build -trimpath -ldflags="-s -w" -o ssmon .
```

Výsledkem je linuxová binárka `ssmon` (bez přípony). Na server ji nahraješ třeba přes
`scp ssmon user@server:~/` nebo WinSCP.

### Linux

```sh
# Ubuntu 24.04+: sudo apt install golang-go
# older distros: official tarball from https://go.dev/dl/
go mod tidy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o ssmon .
```

### Architektura cílového serveru

`uname -m` na serveru: `x86_64` → `GOARCH=amd64`, `aarch64` → `GOARCH=arm64`.

### Ovladač SQLite

| ovladač | build | poznámka |
|---|---|---|
| `modernc.org/sqlite` (výchozí) | `CGO_ENABLED=0 go build …` | čisté Go, statická binárka, snadný cross-compile |
| `github.com/mattn/go-sqlite3` | `CGO_ENABLED=1 go build -tags cgo_sqlite …` | potřebuje gcc, dynamicky linkuje glibc |

---

## Ruční test

Před instalací služby je dobré ověřit, že ssmon reporty vidí.

```sh
# find the interface toward the SpyServer (WireGuard to the RPi)
sudo wg show | grep interface

chmod +x ssmon update-geoip.sh

# GeoIP databases + band plan (one-time)
sudo apt install -y curl
sudo mkdir -p /opt/ssmon
sudo install -m 755 update-geoip.sh /opt/ssmon/
sudo install -m 644 bands.txt /opt/ssmon/
sudo /opt/ssmon/update-geoip.sh

# allow the dashboard only from your own IP
sudo ufw allow from <tvoje-IP> to any port 8090 proto tcp

# test run (database in the current directory, NOT in /tmp - see Troubleshooting)
sudo ./ssmon -iface wg0 -db ./test.db -listen :8090 \
  -initial-mhz 145 -bands /opt/ssmon/bands.txt \
  -geoip-city /opt/ssmon/geoip/dbip-city-lite.mmdb \
  -geoip-asn /opt/ssmon/geoip/dbip-asn-lite.mmdb
```

Očekávaný výstup:
```
ssmon: loaded 22 bands from /opt/ssmon/bands.txt
ssmon: geoip: loaded city database /opt/ssmon/geoip/dbip-city-lite.mmdb (DBIP-City-Lite)
ssmon: geoip: loaded ASN database /opt/ssmon/geoip/dbip-asn-lite.mmdb (DBIP-ASN-Lite ...)
ssmon: database ./test.db: 0 sessions
ssmon: dashboard on :8090
ssmon: capturing directory reports on wg0, TCP port 8080
```

Pak se ke svému SpyServeru připoj (SDR++, SDR#…). Do 15 s se objeví
`session 1 open: <IP> (<město>, <země>, AS… …)`. Na `http://<server>:8090/` uvidíš sebe jako
„Právě poslouchá“. Po odpojení přijde do 15–45 s `session 1 closed`. Test ukončíš přes Ctrl+C
a testovací databázi smažeš (`rm -f test.db*`).

Bez `sudo` jde binárku spustit, když jí dáš oprávnění:
`sudo setcap cap_net_raw+ep ./ssmon`.

---

## Instalace pro ostrý provoz

```sh
sudo install -m 755 ssmon /usr/local/bin/ssmon
sudo mkdir -p /opt/ssmon
sudo install -m 755 update-geoip.sh /opt/ssmon/
sudo install -m 644 bands.txt /opt/ssmon/
sudo /opt/ssmon/update-geoip.sh               # if not done during the test

sudo cp ssmon.service /etc/systemd/system/     # edit -iface etc. if needed
sudo systemctl daemon-reload
sudo systemctl enable --now ssmon

systemctl status ssmon --no-pager
journalctl -u ssmon -f
```

Měsíční aktualizace GeoIP (`sudo crontab -e`):
```
17 4 3 * * /opt/ssmon/update-geoip.sh
```

Služba se spouští po startu serveru a po pádu se do 5 s restartuje. Databáze leží
v `/var/lib/private/ssmon/ssmon.db` (viz [Databáze](#databáze)).

---

## Popis souboru ssmon.service

```ini
[Unit]
Description=SpyServer listener monitor (passive directory report sniffer + dashboard)
After=network-online.target wg-quick@wg0.service   # start after network and WireGuard
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/ssmon \
    -iface wg0 \                          # interface with SpyServer -> directory traffic
    -db /var/lib/ssmon/ssmon.db \         # SQLite DB (physically in /var/lib/private/ssmon)
    -listen :8090 \                       # dashboard on port 8090, all interfaces
    -initial-mhz 145 \                    # SpyServer initial_frequency (not counted)
    -bands /opt/ssmon/bands.txt \         # band plan
    -geoip-city /opt/ssmon/geoip/dbip-city-lite.mmdb \
    -geoip-asn /opt/ssmon/geoip/dbip-asn-lite.mmdb
ExecReload=/bin/kill -HUP $MAINPID        # systemctl reload = reload GeoIP + backfill
Restart=always                            # restart on crash
RestartSec=5
```
*(Komentáře výše jsou jen pro vysvětlení. V `ExecStart` za `\` žádný komentář být nesmí.)*

**Zabezpečení** (sekce `[Service]`):

| direktiva | význam |
|---|---|
| `DynamicUser=yes` | běží pod dočasným neprivilegovaným uživatelem vytvořeným systemd |
| `StateDirectory=ssmon` | systemd vytvoří zapisovatelný `/var/lib/ssmon` (fyzicky `/var/lib/private/ssmon`) |
| `AmbientCapabilities=CAP_NET_RAW` | jediné oprávnění: zachytávání paketů z rozhraní |
| `CapabilityBoundingSet=CAP_NET_RAW` | žádná další oprávnění nelze získat |
| `NoNewPrivileges=yes` | zákaz eskalace oprávnění |
| `ProtectSystem=strict` | celý systém jen pro čtení (kromě `StateDirectory`) |
| `ProtectHome=yes`, `PrivateTmp=yes`, `PrivateDevices=yes` | bez přístupu k /home, vlastní /tmp, žádná zařízení |
| `ProtectKernelTunables/Modules`, `ProtectControlGroups` | bez zásahů do jádra a cgroups |
| `RestrictAddressFamilies=AF_PACKET AF_NETLINK AF_INET AF_INET6 AF_UNIX` | povolené typy socketů: zachytávání (`AF_PACKET`), zjištění rozhraní (`AF_NETLINK`), web a DNS |
| `MemoryMax=200M` | pojistka proti úniku paměti |

Po každé úpravě souboru: `sudo systemctl daemon-reload && sudo systemctl restart ssmon`.

---

## Parametry

| parametr | výchozí | význam |
|---|---|---|
| `-iface` | `wg0` | rozhraní, kterým tečou reporty SpyServeru (prázdné = jen dashboard, bez zachytávání) |
| `-dir-port` | `8080` | TCP port adresáře Airspy |
| `-db` | `/var/lib/ssmon/ssmon.db` | soubor databáze SQLite |
| `-listen` | `:8090` | adresa dashboardu (`:9000`, `10.77.77.1:8090`; prázdné = bez dashboardu) |
| `-geoip-city` | – | City databáze MMDB (DB-IP Lite City nebo GeoLite2-City) |
| `-geoip-asn` | – | ASN databáze MMDB (DB-IP Lite ASN nebo GeoLite2-ASN) |
| `-initial-mhz` | `0` | `initial_frequency` SpyServeru v MHz; první naladění relace na něj se nepočítá (0 = vypnuto) |
| `-bands` | – | soubor s pásmy; bez něj se použije vestavěná tabulka |
| `-interval` | `15s` | interval reportů SpyServeru (pro výpočet délek a stavu online) |
| `-gap` | `45s` | po jaké době bez reportu se relace uzavře |
| `-anonymize-after` | `0` | po N dnech zkrátit IP na /24 (IPv4) nebo /48 (IPv6); 0 = nikdy |
| `-pcap` | – | přehrát soubor `tcpdump -w` do databáze a skončit (pro testování) |

**Signály:** `SIGHUP` (`systemctl reload ssmon`) znovu načte GeoIP databáze a doplní polohu relacím,
které ji nemají. `SIGTERM`/`SIGINT` program korektně ukončí.

---

## GeoIP databáze a jejich aktualizace

ssmon čte soubory MMDB přímo (vlastní čtečka, bez knihoven a bez online API). Skript
`update-geoip.sh` stáhne aktuální **DB-IP Lite City** a **DB-IP Lite ASN** (zdarma, CC BY 4.0),
uloží je do `/opt/ssmon/geoip/` a pošle službě `systemctl reload ssmon`.

```sh
sudo /opt/ssmon/update-geoip.sh     # manual update
```

DB-IP vydává novou verzi na začátku každého měsíce, proto stačí cron 3. dne v měsíci (viz výše).
Skript zkouší aktuální i předchozí měsíc, takže nespadne, když nová verze ještě nevyšla.

**Zpětné doplnění:** relace uložené v době, kdy GeoIP chyběla, dostanou polohu a síť automaticky
při startu programu a po každém `reload`.

Místo DB-IP lze použít i MaxMind GeoLite2 (`GeoLite2-City.mmdb`, `GeoLite2-ASN.mmdb`). Formát je
stejný, stačí změnit cesty v parametrech.

---

## Pásma (bands.txt)

Formát: jeden řádek = `název; od MHz; do MHz; anglický název`, `#` je komentář. První název se
zobrazuje v české verzi dashboardu, nepovinný čtvrtý sloupec v anglické. Bez čtvrtého sloupce se
v obou jazycích použije první název. **Platí první shoda**, užší pásma proto patří nad širší
(např. ISM 433 nad 70 cm).

```
# name;               low;    high;   English name
2 m;                  144;    146;    2 m
Letecké (hlas);       118;    137;    Airband (voice)
ISM 433;              433.05; 434.79; ISM 433
70 cm;                430;    440;    70 cm
```

Po úpravě spusť `sudo systemctl restart ssmon`. Pásmo se počítá až při zobrazení, takže změna platí
i pro všechna stará data. Bez parametru `-bands` se použije vestavěná tabulka (stejná jako
přiložený `bands.txt`).

---

## Dashboard

`http://<server>:8090/` (port podle `-listen`). Data se obnovují každých 15 s.

**Jazyk:** čeština nebo angličtina, přepíná se vlajkami vpravo nahoře. Při první návštěvě se jazyk
řídí prohlížečem (čeština a slovenština → čeština, ostatní → angličtina). Volba se pamatuje
v prohlížeči.

- **stav serveru:** online/offline, čas posledního reportu, aktuální středový kmitočet,
- **„Právě poslouchá“:** místo, IP, síť, kmitočet a délka aktuální relace,
- **souhrnná čísla:** počet relací, unikátních IP a zemí, celkový a průměrný čas poslechu,
- **mapa:** stanice a posluchači (velikost bodu podle počtu relací, zeleně aktuální),
- **statistiky:** země, pásma a kmitočty (MHz) podle času poslechu, aktivita podle hodin a dnů,
  sítě (ASN) s označením hostingových a cloudových sítí (štítek **DC**),
- **tabulka relací:** filtr (IP, město, země, síť, `AS1234`, MHz, název pásma), stránkování po 50,
  posledních 6 kmitočtů v MHz, tlačítko **▸ N×** s celým průběhem ladění, štítek u IP
  s 10 a více relacemi v období (typicky boti),
- přepínač období: 24 h / 7 dní / 30 dní / vše.

Dashboard nemá přihlášení. **Přístup omez firewallem**, např.
`ufw allow from <tvoje-IP> to any port 8090 proto tcp`, nebo ho pusť jen na VPN rozhraní
(`-listen 10.77.77.1:8090`).

### API

| endpoint | popis |
|---|---|
| `GET /api/summary?days=7&tz=3600` | stav serveru a všechny agregace (`days=0` = vše, `tz` = posun od UTC v sekundách) |
| `GET /api/sessions?days=7&q=&limit=50&offset=0` | stránka relací s filtrem |
| `GET /api/session/timeline?id=123` | celý průběh ladění jedné relace |

Všechny endpointy přijímají `lang=cs` (výchozí) nebo `lang=en` pro názvy pásem.

---

## Databáze

Při `DynamicUser=yes` je fyzicky v **`/var/lib/private/ssmon/ssmon.db`** (`/var/lib/ssmon` je na ni
jen odkaz). Vedle leží `ssmon.db-wal` a `ssmon.db-shm` (žurnál SQLite), za běhu je nemaž.
Adresář je přístupný jen rootovi.

| tabulka | obsah |
|---|---|
| `sessions` | jedna řádka za relaci: `ip`, `start`, `last_seen` (unix čas), `reports`, `open`, `cc`, `country`, `city`, `lat`, `lon`, `asn`, `org` |
| `tunings` | změny kmitočtu v relaci: `session_id`, `ts`, `freq` (Hz, přesná hodnota z reportu) |
| `kv` | poslední report (`status`) a čas posledního startu |

Příklady dotazů (`sudo apt install sqlite3`):
```sh
DB=/var/lib/private/ssmon/ssmon.db
sudo sqlite3 -readonly $DB "SELECT cc, count(*) FROM sessions GROUP BY cc ORDER BY 2 DESC"
sudo sqlite3 -readonly $DB "SELECT ip, org, count(*) n FROM sessions GROUP BY ip ORDER BY n DESC LIMIT 10"
sudo sqlite3 -readonly $DB "SELECT datetime(s.start,'unixepoch','localtime'), s.ip, t.freq/1e6
  FROM sessions s JOIN tunings t ON t.session_id = s.id ORDER BY t.ts DESC LIMIT 20"
```

Objem dat je malý: řádově desítky řádků denně, desítky MB za rok i při velkém provozu.

**Záloha za běhu:**
```sh
sudo sqlite3 /var/lib/private/ssmon/ssmon.db ".backup /root/ssmon-backup.db"
```

**Soukromí:** IP adresy posluchačů jsou osobní údaje. Parametr `-anonymize-after 90` je po 90
dnech zkrátí na /24. Poloha a síť zůstanou, statistiky tedy nepřijdou o nic podstatného.

---

## Údržba a aktualizace

| úkon | příkaz |
|---|---|
| nová verze programu | `sudo install -m 755 ssmon /usr/local/bin/ssmon && sudo systemctl restart ssmon` |
| změna parametrů | uprav `/etc/systemd/system/ssmon.service`, pak `sudo systemctl daemon-reload && sudo systemctl restart ssmon` |
| změna pásem | uprav `/opt/ssmon/bands.txt`, pak `sudo systemctl restart ssmon` |
| aktualizace GeoIP | `sudo /opt/ssmon/update-geoip.sh` (běžně z cronu) |
| log | `journalctl -u ssmon -f` |
| stav | `systemctl status ssmon --no-pager` |

Při startu programu se vytvoří schéma databáze, pokud chybí. Nové verze se starou databází
počítají.

---

## Řešení problémů

| příznak | příčina a řešení |
|---|---|
| `packet socket (needs CAP_NET_RAW): operation not permitted` | ruční spuštění bez `sudo`. Použij `sudo` nebo `sudo setcap cap_net_raw+ep ./ssmon`. |
| `capture: route ip+net: netlinkrib: address family not supported by protocol` | v `ssmon.service` chybí `AF_NETLINK` v `RestrictAddressFamilies`. |
| `database … is not writable` / `attempt to write a readonly database` | databáze patří jinému uživateli. Typicky `/tmp/test.db` vytvořená bez `sudo` (Ubuntu brání rootovi zapisovat do cizích souborů v `/tmp`). Smaž ji nebo použij jinou cestu. |
| `listen tcp :8090: bind: address already in use` | port drží jiný proces, často zapomenutý ruční test. `sudo ss -ltnp \| grep 8090`, pak `sudo pkill -f './ssmon'`. |
| `geoip: no database loaded` | chybí parametry `-geoip-city`/`-geoip-asn` nebo soubory. Spusť `update-geoip.sh`. |
| `route ip+net: no such network interface` | špatné jméno rozhraní v `-iface` (`ip -br link`, `sudo wg show`). |
| dashboard „Offline“, žádné relace | reporty nejdou přes zvolené rozhraní. Ověř je: `sudo tcpdump -i wg0 -nn 'tcp port 8080'` (do 15 s musí přijít `POST /register`). Zkontroluj `list_in_directory = 1` ve `spyserver.config`. |
| služba „loaded“, ale web neběží | `systemctl status ssmon` ukáže `activating (auto-restart)`, příčinu najdeš v `journalctl -u ssmon -n 30`. |

Test bez živého provozu: zachycený provoz (`sudo tcpdump -i wg0 -w reporty.pcap 'tcp port 8080'`)
lze přehrát: `./ssmon -pcap reporty.pcap -db ./test.db -iface "" -listen ""`.

---

## Struktura projektu

| soubor | obsah |
|---|---|
| `main.go` | parametry, spuštění, signály |
| `capture.go` | `AF_PACKET` + BPF filtr, přehrávání pcap, skládání TCP, parser reportu |
| `monitor.go` | logika relací, schéma a zápisy do SQLite, anonymizace, zpětné doplnění GeoIP |
| `stats.go` | dotazy pro dashboard (agregace, relace, průběh ladění) |
| `web.go` | HTTP server a API, zabudované statické soubory |
| `geo.go`, `mmdb.go` | GeoIP: vlastní čtečka formátu MaxMind DB |
| `bands.go`, `bands.txt` | tabulka pásem a její načítání |
| `driver_modernc.go`, `driver_cgo.go` | výběr ovladače SQLite (build tag `cgo_sqlite`) |
| `web/index.html` | dashboard (HTML/CSS/JS bez frameworku, texty CZ/EN) |
| `web/leaflet/` | Leaflet 1.9.4 (mapa) |
| `ssmon.service` | systemd unit |
| `update-geoip.sh` | stažení a aktualizace DB-IP Lite |

---

## Licence a uvedení zdrojů

- **Leaflet** 1.9.4: BSD-2-Clause, viz `web/leaflet/LICENSE`.
- **Geolokace:** [IP Geolocation by DB-IP](https://db-ip.com), licence
  [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/). Uvedení zdroje je i v patičce dashboardu.
- **Mapové podklady:** © [OpenStreetMap](https://www.openstreetmap.org/copyright) contributors. Dlaždice
  se načítají z `tile.openstreetmap.org`, platí jejich
  [pravidla používání](https://operations.osmfoundation.org/policies/tiles/).
- SpyServer a Airspy jsou produkty Airspy.com. ssmon s nimi není nijak spojen.
