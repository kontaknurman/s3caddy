# Garage + Caddy Admin Panel

Panel admin untuk mengelola bucket [Garage](https://garagehq.deuxfleurs.fr/) S3
dan domain [Caddy](https://caddyserver.com/) di satu server Linux.

Go murni, **tanpa dependency eksternal** — tidak ada framework web, tidak ada
AWS SDK. Template di-embed dengan `//go:embed`, hasil build adalah satu binary
statis tanpa file pendukung.

```
$ go list -m all
github.com/kontaknurman/s3caddy       # tidak ada baris lain
```

## Isi

- [Yang bisa dilakukan](#yang-bisa-dilakukan)
- [Cara kerja](#cara-kerja)
- [Build](#build)
- [Deploy](#deploy)
  - [1. User `garagepanel`](#1-user-garagepanel)
  - [2. Direktori sites Caddy](#2-direktori-sites-caddy)
  - [3. Sudoers](#3-sudoers)
  - [4. Key S3 untuk panel](#4-key-s3-untuk-panel)
  - [5. File environment](#5-file-environment)
  - [6. Unit systemd](#6-unit-systemd)
- [Akses lewat SSH tunnel](#akses-lewat-ssh-tunnel)
- [Konfigurasi](#konfigurasi)
- [Keamanan](#keamanan)
- [Troubleshooting](#troubleshooting)
- [Pengembangan](#pengembangan)

## Yang bisa dilakukan

| Halaman | Isi |
|---|---|
| **Buckets** | Daftar bucket + ukuran, jumlah objek, status website, jumlah domain. Tambah bucket (4 langkah otomatis), toggle public/private, hapus dengan konfirmasi |
| **Domains** | Daftar domain → bucket, tambah/hapus domain, tulis file Caddy + reload otomatis dengan rollback |
| **Objek** | Browser objek per bucket: grid untuk gambar, list untuk file lain, upload drag-and-drop content-addressed, hapus, copy URL publik |
| **IP Whitelist** | Batasi operasi tulis ke S3 API per IP/CIDR; read (GET/HEAD) selalu terbuka |

## Cara kerja

**Garage Admin API v2.** Semua operasi lewat `/v2/<NamaOperasi>` dengan bearer
token — diverifikasi terhadap
[spesifikasi OpenAPI resmi](https://garagehq.deuxfleurs.fr/api/garage-admin-v2.json)
(v2.3.0). Perlu dicatat:

- Tidak ada endpoint khusus untuk website access. Mengaktifkannya adalah
  `POST /v2/UpdateBucket?id=<id>` dengan body `{"websiteAccess":{"enabled":true,"indexDocument":"index.html"}}`.
- `GET /v2/ListBuckets` **tidak** mengembalikan ukuran atau jumlah objek, jadi
  panel memanggil `GET /v2/GetBucketInfo` per bucket (paralel, maksimal 8
  sekaligus) untuk mengisi kolom itu.

**Routing domain lewat Host header.** Web endpoint Garage (port 3902) memilih
bucket dari header `Host`, bukan dari path. Jadi file Caddy per domain hanya
mengganti Host, tanpa rewrite path sama sekali:

```caddy
# bucket: media
cdn.example.com {
	reverse_proxy 127.0.0.1:3902 {
		header_up Host media
	}
	header Cache-Control "public, max-age=31536000, immutable"
	header X-Content-Type-Options nosniff
	encode gzip zstd
}
```

**Tidak ada database.** Baris `# bucket: media` di atas adalah satu-satunya
sumber pemetaan domain → bucket, dan di-parse ulang dari disk setiap kali
halaman dirender. Hapus filenya, hilang pula domainnya.

**Setiap perubahan config bersifat transaksional.** Panel menulis file secara
atomik (temp file `.tmp-*` + `rename`, jadi Caddy tidak pernah membaca file
setengah jadi), lalu menjalankan `sudo systemctl reload caddy`. Kalau exit code
bukan 0, file dikembalikan persis seperti sebelumnya, reload dijalankan lagi,
dan stderr Caddy ditampilkan ke user. Config yang sedang berjalan tidak pernah
ditinggalkan dalam keadaan rusak.

**SigV4 ditulis tangan** dengan `crypto/hmac` + `crypto/sha256`. Implementasinya
diuji terhadap dua test vector resmi AWS (lihat `s3_test.go`).

## Build

```bash
CGO_ENABLED=0 go build -ldflags="-s -w" -o garagepanel .
```

Hasilnya binary statis ±9 MB tanpa dependency runtime. Untuk build di mesin lain
(misal laptop macOS untuk server Linux):

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o garagepanel .
```

Jalankan test:

```bash
go test ./...
```

## Deploy

Semua perintah di bawah dijalankan sebagai root di server.

Kirim binary ke server dulu:

```bash
# dari mesin lokal
scp garagepanel user@server:/tmp/garagepanel
```

```bash
# di server
install -o root -g root -m 0755 /tmp/garagepanel /usr/local/bin/garagepanel
rm /tmp/garagepanel
```

### 1. User `garagepanel`

User sistem tanpa shell dan tanpa home directory:

```bash
useradd --system --no-create-home --shell /usr/sbin/nologin garagepanel
```

### 2. Direktori sites Caddy

Direktori harus bisa **ditulis hanya oleh `garagepanel`**, dan bisa **dibaca
oleh Caddy**. Bit setgid dipasang supaya file baru otomatis bergrup `caddy`:

```bash
install -d -o garagepanel -g caddy -m 2750 /etc/caddy/sites
```

Pastikan Caddyfile utama mengimpornya:

```bash
grep -q 'import /etc/caddy/sites/\*.caddy' /etc/caddy/Caddyfile \
  || echo 'import /etc/caddy/sites/*.caddy' >> /etc/caddy/Caddyfile
```

> Baris `import` harus berada di level paling atas Caddyfile, bukan di dalam
> blok site.

### 3. Sudoers

Panel hanya boleh menjalankan **satu** perintah sebagai root. Tidak ada wildcard.

Cek dulu di mana `systemctl` berada, karena sudoers mencocokkan path secara
literal:

```bash
command -v systemctl      # biasanya /usr/bin/systemctl atau /bin/systemctl
```

Buat filenya dengan `visudo` supaya syntax error tidak mengunci sudo:

```bash
visudo -f /etc/sudoers.d/garagepanel
```

Isi (sesuaikan path dengan hasil `command -v systemctl`):

```sudoers
garagepanel ALL=(root) NOPASSWD: /bin/systemctl reload caddy
```

Kalau `systemctl` ada di `/usr/bin`, pakai baris itu **dan** set
`CADDY_RELOAD_CMD` (lihat [Konfigurasi](#konfigurasi)), atau daftarkan keduanya:

```sudoers
garagepanel ALL=(root) NOPASSWD: /bin/systemctl reload caddy, /usr/bin/systemctl reload caddy
```

Kunci permissionnya lalu tes sungguhan:

```bash
chmod 0440 /etc/sudoers.d/garagepanel
sudo -u garagepanel sudo -n /bin/systemctl reload caddy && echo "sudoers OK"
```

Kalau muncul `sudo: a password is required`, path di sudoers tidak cocok dengan
yang dipanggil panel.

**Opsional tapi sangat berguna:** izinkan panel membaca journal Caddy supaya
pesan error yang asli (bukan sekadar "Job for caddy.service failed") bisa
ditampilkan di UI saat reload gagal. Ini akses baca saja, bukan privilege escalation:

```bash
usermod -aG systemd-journal garagepanel
```

### 4. Key S3 untuk panel

Panel butuh key S3-nya sendiri untuk membaca isi bucket dan meng-upload objek:

```bash
garage key create garagepanel
```

Catat `Key ID` dan `Secret key` dari output. Untuk **bucket yang sudah ada**,
beri izin satu per satu:

```bash
garage bucket allow --read --write media --key garagepanel
```

Untuk bucket yang dibuat lewat panel, izin ini diberikan otomatis — panel
memanggil `AllowBucketKey` dua kali: sekali untuk `<bucket>-key` yang baru
dibuat, sekali untuk key panel sendiri.

> Panel tetap jalan tanpa key S3, tapi halaman **Objek** dinonaktifkan dengan
> pesan yang jelas. Halaman Buckets, Domains, dan IP Whitelist tidak terpengaruh.

### 5. File environment

Berisi token admin dan secret key, jadi hanya root yang boleh membacanya
(systemd membacanya sebelum privilege di-drop ke `garagepanel`):

```bash
install -d -o root -g root -m 0750 /etc/garagepanel
```

```bash
cat > /etc/garagepanel/garagepanel.env <<'EOF'
GARAGE_ADMIN_TOKEN=ganti-dengan-admin_token-dari-/etc/garage.toml
GARAGE_ADMIN_URL=http://127.0.0.1:3903
GARAGE_S3_URL=http://127.0.0.1:3900
GARAGE_WEB_URL=http://127.0.0.1:3902
GARAGE_S3_ACCESS_KEY=GK...
GARAGE_S3_SECRET_KEY=...
GARAGE_S3_REGION=garage
CADDY_SITES_DIR=/etc/caddy/sites
S3_API_DOMAIN=s3.domainmu.com
LISTEN=127.0.0.1:8090
EOF
```

```bash
chown root:root /etc/garagepanel/garagepanel.env
chmod 0600 /etc/garagepanel/garagepanel.env
```

`GARAGE_ADMIN_TOKEN` diambil dari blok `[admin]` di `/etc/garage.toml`:

```bash
grep -A2 '^\[admin\]' /etc/garage.toml
```

> Nilai di `EnvironmentFile` **tidak** boleh diapit tanda kutip kecuali tanda
> kutipnya memang bagian dari nilai — systemd tidak menghapusnya seperti shell.

### 6. Unit systemd

```bash
install -o root -g root -m 0644 garagepanel.service /etc/systemd/system/garagepanel.service
systemctl daemon-reload
systemctl enable --now garagepanel
systemctl status garagepanel
```

Cek log:

```bash
journalctl -u garagepanel -f
```

Saat start yang sehat:

```
garagepanel: terhubung ke Garage Admin API di http://127.0.0.1:3903
garagepanel: siap di http://127.0.0.1:8090 (loopback saja — akses lewat SSH tunnel)
```

## Akses lewat SSH tunnel

Panel **hanya** mendengarkan di loopback, jadi tidak bisa dibuka langsung dari
internet. Dari mesin lokal:

```bash
ssh -N -L 8090:127.0.0.1:8090 user@server
```

Lalu buka <http://127.0.0.1:8090> di browser. Tutup tunnel dengan `Ctrl-C`.

Kalau port 8090 di laptop sudah terpakai, petakan ke port lain — panel menerima
Host `localhost`/`127.0.0.1` di port berapa pun:

```bash
ssh -N -L 9999:127.0.0.1:8090 user@server   # buka http://127.0.0.1:9999
```

Entry `~/.ssh/config` supaya tidak perlu mengetik ulang:

```sshconfig
Host garage-panel
    HostName server.domainmu.com
    User user
    LocalForward 8090 127.0.0.1:8090
    RequestTTY no
    SessionType none
```

```bash
ssh garage-panel
```

## Konfigurasi

Semua lewat environment variable.

| Variabel | Default | Keterangan |
|---|---|---|
| `GARAGE_ADMIN_TOKEN` | — | **Wajib.** Panel gagal start dengan pesan jelas kalau kosong |
| `GARAGE_ADMIN_URL` | `http://127.0.0.1:3903` | Admin API |
| `GARAGE_S3_URL` | `http://127.0.0.1:3900` | S3 API (SigV4) |
| `GARAGE_WEB_URL` | `http://127.0.0.1:3902` | Web endpoint, dipilih per bucket lewat header Host |
| `GARAGE_S3_ACCESS_KEY` | — | Key S3 panel. Kosong → halaman Objek nonaktif |
| `GARAGE_S3_SECRET_KEY` | — | Secret key panel |
| `GARAGE_S3_REGION` | `garage` | Region untuk signature SigV4 |
| `CADDY_SITES_DIR` | `/etc/caddy/sites` | Direktori file `*.caddy` |
| `S3_API_DOMAIN` | — | Domain S3 API. Kosong → halaman IP Whitelist nonaktif |
| `LISTEN` | `127.0.0.1:8090` | **Wajib loopback.** Alamat non-loopback ditolak saat start |
| `CADDY_RELOAD_CMD` | `sudo -n /bin/systemctl reload caddy` | Perintah reload. Dipecah per spasi, **tidak** lewat shell |

## Keamanan

Panel ini menulis file config dan me-reload systemd service, jadi punya hak
istimewa. Yang sudah dipasang:

**Binding.** Hanya loopback. `LISTEN=0.0.0.0:8090` ditolak saat start dengan
pesan eksplisit; tidak ada opsi untuk membukanya ke jaringan.

**Host header.** Request dengan Host selain `localhost`/`127.0.0.1`/`::1`
dijawab 403. Ini menahan serangan DNS rebinding dari halaman web lain yang
kebetulan terbuka di browser user.

**CSRF.** Panel tidak punya login, jadi tanpa proteksi ini halaman mana pun di
browser user bisa mem-POST ke port yang ditunnel dan mengubah config Caddy.
Setiap request non-GET wajib membawa token (double-submit cookie, cookie
`HttpOnly` + `SameSite=Strict`), dan header `Origin` divalidasi kalau ada.

**Validasi input.** Semua string yang masuk ke nama file atau isi Caddyfile
divalidasi lebih dulu:

| Field | Aturan |
|---|---|
| Nama bucket | `^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$` |
| Domain | `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$` |
| IP / CIDR | `net.ParseIP` dan `net.ParseCIDR` — bukan regex; hasilnya dinormalkan (`198.51.100.5/24` → `198.51.100.0/24`) |
| Label | `[a-zA-Z0-9 _-]`, maks 40 karakter (`\|` sengaja dilarang karena jadi pemisah di komentar) |
| Object key | tolak yang mengandung `..` atau diawali `/` |

Semua nilai yang dipakai sebagai komponen nama file juga ditolak kalau
mengandung `..`, `/`, `\`, diawali titik, atau berisi karakter kontrol.

**Output.** Semua HTML lewat `html/template` dengan auto-escape kontekstual.
`text/template` tidak dipakai untuk output web sama sekali.

**Preview objek.** Endpoint `/preview` mem-proxy lewat panel, bukan lewat URL
publik, jadi bucket private pun tetap bisa dilihat (kalau web endpoint menolak
karena website access mati, panel fallback ke `GetObject` bertandatangan).
Karena isinya disajikan dari origin panel sendiri, **hanya gambar raster**
(`jpg`, `jpeg`, `png`, `webp`, `gif`, `avif`) yang boleh tampil inline; tipe
lain — termasuk `.html` dan `.svg` yang bisa mengeksekusi script — dipaksa
jadi `application/octet-stream` + `Content-Disposition: attachment`, ditambah
`X-Content-Type-Options: nosniff` dan CSP `default-src 'none'; sandbox`.

**Upload.** Maksimal 50 MB per file, whitelist ekstensi, dan nama objek
sepenuhnya dibuat dari isi file (`sha256(isi)[:16] + ekstensi`) — nama asli dari
client dibuang, jadi tidak ada jalur dari nama file user ke nama objek.

**Privilege.** Jalan sebagai user non-root `garagepanel`, dengan sudoers untuk
persis satu perintah tanpa wildcard. Unit systemd sengaja memakai
`NoNewPrivileges=no` (sudo butuh setuid) — ini dijelaskan di komentar unit
supaya tidak "diperbaiki" jadi `yes` lalu reload rusak diam-diam.

**Secret.** Token admin dan secret key tidak pernah ditulis ke log. Error dari
transport pun dibersihkan dari URL lengkap sebelum ditampilkan. Access log hanya
mencatat method, path, status, durasi — tanpa query string dan tanpa header.

**Yang tidak dilakukan panel ini:** tidak ada autentikasi user. Siapa pun yang
bisa membuka port loopback di server itu (yaitu siapa pun yang punya akses SSH)
punya kendali penuh atas panel. Itu memang modelnya — amankan akses SSH-nya.

## Troubleshooting

**`GARAGE_ADMIN_TOKEN belum diisi`** — panel sengaja tidak mau start. Cek
`/etc/garagepanel/garagepanel.env` terbaca oleh systemd:
`systemctl show garagepanel -p Environment`.

**`token admin ditolak — periksa GARAGE_ADMIN_TOKEN`** — token tidak cocok
dengan `admin_token` di `/etc/garage.toml`. Garage perlu di-restart kalau
tokennya baru diubah.

**Reload Caddy gagal, pesannya cuma "Job for caddy.service failed"** — panel
sudah mencoba membaca journal Caddy untuk menampilkan error aslinya. Kalau
bagian "journal caddy" kosong, tambahkan user ke grup journal:
`usermod -aG systemd-journal garagepanel && systemctl restart garagepanel`.

**`sudo: a password is required`** — path `systemctl` di sudoers tidak sama
dengan yang dipanggil panel. Bandingkan `command -v systemctl` dengan
`CADDY_RELOAD_CMD`.

**`tidak bisa menulis di /etc/caddy/sites`** — cek kepemilikan direktori:
`stat -c '%U %G %a' /etc/caddy/sites` harus `garagepanel caddy 2750`.

**Halaman Objek bilang kredensial belum diatur** — `GARAGE_S3_ACCESS_KEY` /
`GARAGE_S3_SECRET_KEY` kosong di env file.

**Bucket muncul di daftar tapi objeknya kosong / `AccessDenied`** — key panel
belum diberi izin pada bucket lama:
`garage bucket allow --read --write <bucket> --key garagepanel`.

**Domain sudah ditambah tapi masih 404** — pastikan DNS-nya sudah mengarah ke
server ini (Caddy butuh itu untuk menerbitkan sertifikat), dan bucket-nya
`public` (website access aktif). Cek langsung tanpa lewat Caddy:

```bash
curl -H "Host: media" http://127.0.0.1:3902/index.html
```

**Gambar tidak muncul di grid** — buka satu URL preview langsung untuk melihat
pesan errornya: `curl -i "http://127.0.0.1:8090/preview?bucket=media&key=namafile.jpg"`.

## Pengembangan

```
main.go               konfigurasi, routing, middleware, semua handler
garage.go             client Garage Admin API v2
s3.go                 SigV4, ListObjectsV2, PutObject, GetObject, StatObject, DeleteObject
caddy.go              tulis/hapus file site + reload + rollback, file whitelist
validate.go           semua validasi input
templates/            *.html, di-embed dengan //go:embed
garagepanel.service   unit systemd
```

Test mencakup vector SigV4 resmi AWS, round-trip parse/render file Caddy,
rollback saat reload gagal, dan integrasi seluruh handler terhadap Garage
tiruan (urutan pemanggilan API, CSRF, escaping HTML, content-addressing upload,
proteksi path traversal):

```bash
go test ./...          # cepat
go test -race ./...    # dengan race detector
go test -cover ./...   # ~75% statement coverage
```

Menjalankan panel lokal tanpa server Garage sungguhan: lihat
`newTestPanel` di `main_test.go` — isinya Garage + S3 + web endpoint tiruan yang
bisa dipakai ulang.
