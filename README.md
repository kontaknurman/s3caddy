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
- [Peta komponen](#peta-komponen)
- [Memasang Go dan Caddy](#memasang-go-dan-caddy)
  - [Go (di mesin build)](#go-di-mesin-build)
  - [Caddy (di server)](#caddy-di-server)
- [Instalasi](#instalasi)
  - [Prasyarat](#prasyarat)
  - [Langkah 0 — Periksa Garage dan Caddy](#langkah-0--periksa-garage-dan-caddy)
  - [Langkah 1 — Build binary](#langkah-1--build-binary)
  - [Langkah 2 — Kirim binary ke server](#langkah-2--kirim-binary-ke-server)
  - [Langkah 3 — Buat user `garagepanel`](#langkah-3--buat-user-garagepanel)
  - [Langkah 4 — Siapkan direktori sites Caddy](#langkah-4--siapkan-direktori-sites-caddy)
  - [Langkah 5 — Pasang sudoers](#langkah-5--pasang-sudoers)
  - [Langkah 6 — Buat key S3 untuk panel](#langkah-6--buat-key-s3-untuk-panel)
  - [Langkah 7 — Tulis file environment](#langkah-7--tulis-file-environment)
  - [Langkah 8 — Pasang unit systemd](#langkah-8--pasang-unit-systemd)
  - [Langkah 9 — Buka panel lewat SSH tunnel](#langkah-9--buka-panel-lewat-ssh-tunnel)
  - [Langkah 10 — Uji coba end-to-end](#langkah-10--uji-coba-end-to-end)
  - [Checklist instalasi](#checklist-instalasi)
- [Login dan akses lewat domain](#login-dan-akses-lewat-domain)
- [Update ke versi baru](#update-ke-versi-baru)
- [Backup dan pemulihan](#backup-dan-pemulihan)
- [Uninstall](#uninstall)
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

## Peta komponen

Semua berjalan di satu server. Yang terekspos ke internet hanya Caddy; Garage dan
panel seluruhnya di loopback.

**Jalur admin — kamu memakai panel:**

```
  browser ──SSH tunnel──────────────▶ 127.0.0.1:8090  garagepanel
  browser ──https──▶ Caddy ─────────▶ 127.0.0.1:8090  garagepanel
                   (opsional, butuh login)     │
                                               ├──▶ 127.0.0.1:3903  Garage admin
                                               │       bucket, key, website access
                                               ├──▶ 127.0.0.1:3900  Garage S3
                                               │       daftar/unggah/hapus objek (SigV4)
                                               ├──▶ 127.0.0.1:3902  Garage web
                                               │       thumbnail lewat /preview
                                               │
                                               └──▶ tulis /etc/caddy/sites/*.caddy
                                                       lalu `systemctl reload caddy`
```

**Jalur pengunjung — orang membuka domainmu:**

```
  browser ──https──▶ Caddy ──Host: <nama-bucket>──▶ 127.0.0.1:3902  Garage web
```

| Port | Komponen | Diikat ke | Dipakai untuk |
|---|---|---|---|
| 3900 | Garage S3 API | `127.0.0.1` | Panel: daftar, unggah, hapus objek (SigV4). Opsional diekspos Caddy lewat `_s3api.caddy` |
| 3902 | Garage web | `127.0.0.1` | Caddy: melayani bucket per domain. Panel: thumbnail lewat `/preview` |
| 3903 | Garage admin | `127.0.0.1` | Panel: bucket, key, website access |
| 8090 | garagepanel | `127.0.0.1` | UI panel — lewat SSH tunnel, atau lewat Caddy kalau login diaktifkan |
| 80, 443 | Caddy | publik | Domain bucket, sertifikat TLS otomatis |

Yang perlu ada di mesin mana:

| | Mesin build | Server |
|---|---|---|
| Go 1.24+ | ✔ | — (binary statis) |
| Garage | — | ✔ |
| Caddy | — | ✔ |
| garagepanel | dibuat di sini | ✔ dijalankan di sini |

## Memasang Go dan Caddy

Lewati bagian ini kalau keduanya sudah ada. Go hanya dibutuhkan di **mesin yang
mem-build** — binary hasilnya statis, jadi server tidak perlu Go terpasang.
Caddy dibutuhkan di **server**.

Memasang Garage sendiri di luar cakupan dokumen ini; ikuti
[panduan resminya](https://garagehq.deuxfleurs.fr/documentation/quick-start/)
kalau belum ada.

### Go (di mesin build)

Panel butuh **Go 1.24 atau lebih baru** — hash password memakai `crypto/pbkdf2`
yang baru masuk pustaka standar di 1.24. Paket bawaan distro (`apt install
golang-go`) sering tertinggal jauh, jadi lebih aman pasang dari go.dev.

**Linux.** Perintah ini mengambil versi stabil terkini secara otomatis:

```bash
GO_VERSION=$(curl -sL 'https://go.dev/VERSION?m=text' | head -1)
echo "akan memasang $GO_VERSION"

curl -LO "https://go.dev/dl/${GO_VERSION}.linux-amd64.tar.gz"
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf "${GO_VERSION}.linux-amd64.tar.gz"
rm "${GO_VERSION}.linux-amd64.tar.gz"
```

> Ganti `amd64` dengan `arm64` kalau mesinnya ARM. Cek dengan `uname -m`:
> `x86_64` → `amd64`, `aarch64` → `arm64`.
>
> `rm -rf /usr/local/go` itu memang bagian dari prosedur resmi — menimpa
> instalasi lama tanpa menghapusnya dulu bisa meninggalkan file campuran.

Tambahkan ke `PATH` untuk semua user, lalu muat di shell yang sedang terbuka:

```bash
echo 'export PATH=$PATH:/usr/local/go/bin' | sudo tee /etc/profile.d/go.sh
. /etc/profile.d/go.sh
```

**macOS.** Paling ringkas lewat Homebrew:

```bash
brew install go
```

Tanpa Homebrew, unduh paket `.pkg` dari <https://go.dev/dl/> lalu jalankan —
installer-nya memasang ke `/usr/local/go` dan mengatur `PATH` sendiri. Buka
terminal baru setelahnya.

**Verifikasi** (di shell baru):

```bash
go version
```

Harus keluar `go1.24` atau lebih baru. Kalau `command not found`, `PATH`-nya
belum termuat — buka terminal baru, atau jalankan ulang `. /etc/profile.d/go.sh`.

### Caddy (di server)

Pakai repositori resmi, bukan paket bawaan distro — versi di repo distro
biasanya tertinggal dan tidak punya HTTPS otomatis yang dikonfigurasi rapi.

**Debian / Ubuntu / Raspbian:**

```bash
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
  | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
  | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo chmod o+r /usr/share/keyrings/caddy-stable-archive-keyring.gpg
sudo chmod o+r /etc/apt/sources.list.d/caddy-stable.list
sudo apt update
sudo apt install caddy
```

**Fedora:**

```bash
sudo dnf install dnf5-plugins
sudo dnf copr enable @caddy/caddy
sudo dnf install caddy
```

**RHEL / CentOS / Rocky / Alma:**

```bash
sudo dnf install dnf-plugins-core
sudo dnf copr enable @caddy/caddy
sudo dnf install caddy
```

**Distro lain:** unduh binary statis dari
<https://github.com/caddyserver/caddy/releases>, taruh di `/usr/local/bin/caddy`,
lalu pasang unit systemd-nya mengikuti
[panduan resmi Caddy](https://caddyserver.com/docs/running#manual-installation).
Yang penting untuk panel ini: service-nya harus berjalan sebagai user `caddy`
dan membaca `/etc/caddy/Caddyfile`.

**Verifikasi.** Paket apt dan dnf sudah sekalian memasang unit systemd-nya:

```bash
caddy version
systemctl is-enabled caddy
systemctl is-active caddy
systemctl show caddy -p User --value    # harus: caddy
```

Kalau belum aktif:

```bash
sudo systemctl enable --now caddy
```

Setelah terpasang, Caddy melayani halaman selamat datang di port 80. Konfigurasinya
ada di `/etc/caddy/Caddyfile`; baris `import` yang dibutuhkan panel ditambahkan
nanti di [Langkah 4](#langkah-4--siapkan-direktori-sites-caddy).

**Buka port 80 dan 443 di firewall.** Caddy butuh keduanya: 443 untuk melayani
domain, dan 80 untuk verifikasi ACME saat menerbitkan sertifikat TLS. Panel
sendiri tidak butuh port apa pun terbuka — aksesnya lewat SSH tunnel.

```bash
sudo ufw allow 80,443/tcp        # Debian/Ubuntu dengan ufw
# atau
sudo firewall-cmd --permanent --add-service={http,https} && sudo firewall-cmd --reload
```

## Instalasi

Tutorial dari nol sampai panel bisa dipakai. Sekitar 15 menit. Setiap langkah
punya perintah verifikasi — jangan lanjut kalau hasilnya tidak sesuai.

Perintah dengan `sudo` dijalankan di server; sisanya di mesin lokal (kalau tidak
dijelaskan, artinya di server).

### Prasyarat

| Kebutuhan | Cara cek | Hasil yang benar |
|---|---|---|
| Garage v2 sudah jalan | `garage status` | daftar node, tanpa error |
| Caddy sudah jalan | `systemctl is-active caddy` | `active` |
| Akses `sudo` di server | `sudo -v` | tidak error |
| Go 1.24+ di mesin build | `go version` | `go1.24` atau lebih baru |

Belum ada Go atau Caddy? Lihat [Memasang Go dan Caddy](#memasang-go-dan-caddy)
di atas.

Panel **tidak** memasang atau mengubah Garage maupun Caddy. Keduanya harus sudah
berjalan lebih dulu.

Kalau di mesin lokal tidak ada Go, semua langkah build bisa dikerjakan langsung
di server — lihat catatan di [Langkah 1](#langkah-1--build-binary).

### Langkah 0 — Periksa Garage dan Caddy

Langkah ini tidak mengubah apa pun, hanya memastikan yang dibutuhkan panel sudah
menyala. Lewati kalau kamu sudah yakin.

**0a. Garage hidup**

```bash
systemctl is-active garage && garage status
```

**0b. Admin API menyala**

```bash
sudo grep -A5 '^\[admin\]' /etc/garage.toml
```

Yang dicari:

```toml
[admin]
api_bind_addr = "127.0.0.1:3903"
admin_token = "…"
```

Kalau blok `[admin]` tidak ada, tambahkan lalu `sudo systemctl restart garage`:

```toml
[admin]
api_bind_addr = "127.0.0.1:3903"
admin_token = "ganti-dengan-nilai-acak-panjang"
```

Nilai acak untuk `admin_token` bisa dibuat dengan `openssl rand -base64 32`.

**0c. Siapkan token admin**

Ada dua pilihan.

*Pilihan A — pakai `admin_token` dari config.* Paling cepat. Token ini punya
scope penuh dan tidak pernah kedaluwarsa:

```bash
sudo grep '^admin_token' /etc/garage.toml
```

*Pilihan B — token khusus panel (disarankan).* Bisa dibatasi scope-nya dan bisa
dicabut tanpa mengganggu yang lain. Panel hanya memakai delapan operasi:

```bash
garage admin-token create --expires-in 365d \
  --scope GetClusterHealth,ListBuckets,GetBucketInfo,CreateBucket,DeleteBucket,UpdateBucket,CreateKey,AllowBucketKey \
  garagepanel
```

> Garage menampilkan token ini **sekali saja** — salin sekarang.
>
> Perhatikan `--expires-in`: begitu kedaluwarsa, panel berhenti bisa bicara
> dengan Garage. Cek `garage admin-token --help` untuk opsi tanpa kedaluwarsa,
> atau pakai Pilihan A, atau pasang pengingat untuk memperpanjang.

Uji tokennya sekarang juga — ini sekaligus membuktikan Admin API bisa dihubungi:

```bash
curl -s -H "Authorization: Bearer TOKEN_KAMU" http://127.0.0.1:3903/v2/GetClusterHealth
```

Harus keluar JSON berisi `"status"`. Kalau `401`, tokennya salah. Kalau
`Connection refused`, `api_bind_addr` belum aktif (ulangi 0b).

**0d. Web endpoint menyala**

```bash
sudo grep -A4 '^\[s3_web\]' /etc/garage.toml
curl -s -o /dev/null -w '%{http_code}\n' -H "Host: bucket-yang-tidak-ada" http://127.0.0.1:3902/
```

Jawaban `404` justru bagus: artinya endpoint hidup dan menolak bucket yang
memang tidak ada. Yang salah adalah `000` / `Connection refused`.

Kalau blok `[s3_web]` tidak ada, tambahkan lalu restart Garage:

```toml
[s3_web]
bind_addr = "127.0.0.1:3902"
```

Itu saja yang dibutuhkan. Dokumen index (`index.html`) **bukan** setelan global —
ia diatur per bucket, dan panel yang mengisinya lewat `UpdateBucket` setiap kali
kamu mencentang "Public".

`root_domain` di `[s3_web]` **tidak wajib** untuk panel ini. Garage melayani
bucket kalau Host cocok dengan `<bucket>.<root_domain>` **atau** persis sama
dengan nama bucket — yang kedua itulah yang dipakai panel lewat
`header_up Host <bucket>`.

**0d-2. Jam server harus akurat.** Panel menandatangani request S3 dengan SigV4,
dan tanda tangan itu memuat stempel waktu. Kalau jam server meleset lebih dari
sekitar 15 menit, Garage menolak semuanya dengan `RequestTimeTooSkewed` dan
halaman Objek akan kosong tanpa sebab yang jelas.

```bash
timedatectl status | grep -E 'System clock|NTP service'
```

Harus `System clock synchronized: yes` dan `NTP service: active`. Kalau belum:
`sudo timedatectl set-ntp true`.

**0e. Caddy mengimpor direktori sites**

```bash
systemctl is-active caddy
grep -n 'import' /etc/caddy/Caddyfile
```

Harus ada baris `import /etc/caddy/sites/*.caddy` di level paling atas (bukan di
dalam blok site). Kalau belum ada, ditangani di [Langkah 4](#langkah-4--siapkan-direktori-sites-caddy).

### Langkah 1 — Build binary

Di mesin lokal:

```bash
git clone https://github.com/kontaknurman/s3caddy.git
cd s3caddy
CGO_ENABLED=0 go build -ldflags="-s -w" -o garagepanel .
```

Kalau mesin lokal bukan Linux amd64 (misal MacBook Apple Silicon untuk server
Linux), tentukan targetnya:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o garagepanel .
```

Verifikasi:

```bash
file garagepanel
```

Harus tertulis `ELF 64-bit … statically linked`. `CGO_ENABLED=0` itu yang membuat
binary-nya statis, jadi tidak ada urusan versi glibc di server.

> **Tidak ada Go di mesin lokal?** Clone repo-nya di server, build di sana, lalu
> lompat ke [Langkah 3](#langkah-3--buat-user-garagepanel) — Langkah 2 tidak
> diperlukan. Setelah selesai, Go boleh dihapus lagi dari server; binary-nya
> tidak membutuhkannya saat jalan.

Sekalian jalankan test-nya (opsional, ±2 detik):

```bash
go test ./...
```

### Langkah 2 — Kirim binary ke server

```bash
scp garagepanel user@server:/tmp/garagepanel
```

Lalu di server, pasang ke `/usr/local/bin` sebagai milik root — panel tidak boleh
bisa menimpa binary-nya sendiri:

```bash
sudo install -o root -g root -m 0755 /tmp/garagepanel /usr/local/bin/garagepanel
rm /tmp/garagepanel
```

Verifikasi — jalankan tanpa konfigurasi apa pun:

```bash
/usr/local/bin/garagepanel
```

Harus keluar pesan ini lalu berhenti dengan exit code 1:

```
Konfigurasi tidak lengkap:

GARAGE_ADMIN_TOKEN belum diisi.
…
```

Itu tandanya binary-nya jalan di server ini.

### Langkah 3 — Buat user `garagepanel`

User sistem tanpa shell dan tanpa home directory:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin garagepanel
id garagepanel
```

Kalau `useradd` bilang user sudah ada, lanjut saja.

### Langkah 4 — Siapkan direktori sites Caddy

Direktori ini harus bisa **ditulis hanya oleh `garagepanel`** dan bisa **dibaca
oleh Caddy**. Cek dulu Caddy jalan sebagai user apa:

```bash
systemctl show caddy -p User --value    # biasanya: caddy
```

Buat direktorinya (ganti `caddy` kalau hasil di atas berbeda). Bit setgid (`2`
di depan `750`) membuat setiap file baru otomatis bergrup `caddy`, supaya Caddy
bisa membacanya:

```bash
sudo install -d -o garagepanel -g caddy -m 2750 /etc/caddy/sites
stat -c '%U %G %a' /etc/caddy/sites
```

Harus keluar persis `garagepanel caddy 2750`.

Pastikan Caddyfile utama mengimpornya:

```bash
grep -q 'import /etc/caddy/sites/\*.caddy' /etc/caddy/Caddyfile \
  || echo 'import /etc/caddy/sites/*.caddy' | sudo tee -a /etc/caddy/Caddyfile
```

Uji bahwa Caddy masih mau reload dengan direktori yang masih kosong:

```bash
sudo systemctl reload caddy && echo "reload OK"
```

> Kalau Caddy mengeluh soal pola import yang tidak cocok dengan file mana pun,
> isi dengan file kosong sebagai placeholder:
> `printf '# placeholder\n' | sudo tee /etc/caddy/sites/_placeholder.caddy`
> lalu reload lagi. Nama berawalan `_` sengaja dipilih supaya panel
> mengabaikannya (lihat [Cara kerja](#cara-kerja)).

### Langkah 5 — Pasang sudoers

Panel hanya boleh menjalankan **satu** perintah sebagai root. Tidak ada wildcard.

Cek dulu di mana `systemctl` berada — sudoers mencocokkan path secara literal,
dan ini penyebab kegagalan paling sering di tahap ini:

```bash
command -v systemctl
```

Buat filenya dengan `visudo` supaya syntax error tidak mengunci sudo:

```bash
sudo visudo -f /etc/sudoers.d/garagepanel
```

Isi — pakai **path hasil perintah di atas**:

```sudoers
garagepanel ALL=(root) NOPASSWD: /usr/bin/systemctl reload caddy
```

Kalau tidak yakin, daftarkan dua-duanya:

```sudoers
garagepanel ALL=(root) NOPASSWD: /bin/systemctl reload caddy, /usr/bin/systemctl reload caddy
```

Kunci permissionnya lalu **tes sungguhan** sebagai user panel:

```bash
sudo chmod 0440 /etc/sudoers.d/garagepanel
sudo -u garagepanel sudo -n /bin/systemctl reload caddy && echo "sudoers OK"
```

- `sudo: a password is required` → path di sudoers tidak cocok dengan yang
  dipanggil panel. Samakan, atau set `CADDY_RELOAD_CMD` di Langkah 7.
- `sudo: no tty present` → sama, path-nya belum cocok.

**Opsional tapi sangat berguna:** izinkan panel membaca journal Caddy, supaya
saat reload gagal yang tampil di UI adalah pesan error Caddy yang asli, bukan
sekadar "Job for caddy.service failed". Ini akses baca saja, bukan privilege
escalation:

```bash
sudo usermod -aG systemd-journal garagepanel
```

### Langkah 6 — Buat key S3 untuk panel

Panel butuh key S3-nya sendiri untuk membaca isi bucket dan meng-upload objek:

```bash
garage key create garagepanel
```

Catat `Key ID` (diawali `GK…`) dan `Secret key` dari output — secret-nya hanya
ditampilkan sekali.

Untuk **bucket yang sudah ada**, beri izin satu per satu:

```bash
garage bucket list
garage bucket allow --read --write media --key garagepanel
```

Untuk bucket yang nanti dibuat lewat panel, izin ini diberikan otomatis — panel
memanggil `AllowBucketKey` dua kali: sekali untuk `<bucket>-key` yang baru
dibuat, sekali untuk key panel sendiri.

> Panel tetap jalan tanpa key S3, tapi halaman **Objek** dinonaktifkan dengan
> pesan yang jelas. Halaman Buckets, Domains, dan IP Whitelist tidak terpengaruh.

### Langkah 7 — Tulis file environment

File ini berisi token admin dan secret key, jadi hanya root yang boleh
membacanya — systemd membacanya sebelum privilege di-drop ke `garagepanel`:

```bash
sudo install -d -o root -g root -m 0750 /etc/garagepanel
sudo tee /etc/garagepanel/garagepanel.env > /dev/null <<'EOF'
GARAGE_ADMIN_TOKEN=token-dari-langkah-0c
GARAGE_ADMIN_URL=http://127.0.0.1:3903
GARAGE_S3_URL=http://127.0.0.1:3900
GARAGE_WEB_URL=http://127.0.0.1:3902
GARAGE_S3_ACCESS_KEY=GK-dari-langkah-6
GARAGE_S3_SECRET_KEY=secret-dari-langkah-6
GARAGE_S3_REGION=garage
CADDY_SITES_DIR=/etc/caddy/sites
S3_API_DOMAIN=s3.domainmu.com
LISTEN=127.0.0.1:8090

# Login panel — opsional, lihat bagian "Login dan akses lewat domain".
# Kosongkan kalau panel hanya diakses lewat SSH tunnel.
#PANEL_USERNAME=admin
#PANEL_PASSWORD_HASH=
#PANEL_DOMAIN=
EOF
sudo chmod 0600 /etc/garagepanel/garagepanel.env
```

Lalu isi nilai yang sebenarnya:

```bash
sudo nano /etc/garagepanel/garagepanel.env
stat -c '%U %G %a' /etc/garagepanel/garagepanel.env    # harus: root root 600
```

Catatan pengisian:

- **Jangan pakai tanda kutip** kecuali tanda kutipnya memang bagian dari nilai —
  systemd tidak menghapusnya seperti shell.
- `S3_API_DOMAIN` boleh dikosongkan; halaman IP Whitelist akan nonaktif dengan
  pesan yang jelas.
- Kalau `command -v systemctl` di Langkah 5 bukan `/bin/systemctl`, tambahkan
  satu baris: `CADDY_RELOAD_CMD=sudo -n /usr/bin/systemctl reload caddy`.
- Baris `PANEL_*` boleh tetap dikomentari untuk sekarang. Panel berjalan tanpa
  login dan hanya bisa dibuka lewat SSH tunnel — itu bawaannya. Aktifkan nanti
  lewat [Login dan akses lewat domain](#login-dan-akses-lewat-domain) kalau
  memang perlu.

Kesalahan paling sering di file ini adalah tanda kutip yang tidak sengaja
terbawa. Perintah ini harus tidak mengeluarkan hasil apa pun:

```bash
sudo grep -n '"' /etc/garagepanel/garagepanel.env
```

Isinya baru benar-benar diuji di Langkah 8, lewat systemd yang membaca file ini
dengan aturan parsing-nya sendiri.

### Langkah 8 — Pasang unit systemd

```bash
sudo install -o root -g root -m 0644 garagepanel.service /etc/systemd/system/garagepanel.service
sudo systemctl daemon-reload
sudo systemctl enable --now garagepanel
```

Verifikasi:

```bash
systemctl is-active garagepanel        # harus: active
journalctl -u garagepanel -n 20 --no-pager
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8090/buckets   # harus: 200
```

> Unit ini sengaja memakai `NoNewPrivileges=no` — sudo butuh setuid. Kalau
> diubah jadi `yes`, reload Caddy akan selalu gagal. Alasannya ditulis juga
> sebagai komentar di dalam file unit-nya.

### Langkah 9 — Buka panel lewat SSH tunnel

Panel **hanya** mendengarkan di loopback, jadi tidak bisa dibuka langsung dari
internet dan tidak perlu port apa pun dibuka di firewall. Dari mesin lokal:

```bash
ssh -N -L 8090:127.0.0.1:8090 user@server
```

Biarkan terminal itu terbuka, lalu buka <http://127.0.0.1:8090> di browser.
Tutup tunnel dengan `Ctrl-C`.

Kalau port 8090 di laptop sudah terpakai, petakan ke port lain — panel menerima
Host `localhost`/`127.0.0.1` di port berapa pun:

```bash
ssh -N -L 9999:127.0.0.1:8090 user@server   # buka http://127.0.0.1:9999
```

Supaya tidak perlu mengetik ulang, tambahkan ke `~/.ssh/config`:

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

### Langkah 10 — Uji coba end-to-end

Sekarang buktikan semuanya nyambung. Semua lewat UI di browser.

**1. Buat bucket.** Di halaman **Buckets**, isi nama `uji-panel`, centang
**Public**, klik **Buat bucket**. Semua langkah harus bercentang hijau:

```
✓ CreateBucket "uji-panel"
✓ CreateKey "uji-panel-key"
✓ AllowBucketKey (read+write)
✓ AllowBucketKey untuk key panel sendiri (read+write)
✓ Aktifkan website access (index: index.html)
```

Access key dan secret muncul sekali di sini — untuk bucket uji coba boleh
diabaikan.

**2. Upload.** Buka **Objek** → pilih bucket `uji-panel` → tarik satu file gambar
ke kotak upload. Statusnya harus jadi `selesai → <16 hex>.png`, dan
thumbnail-nya muncul di grid. Kalau thumbnail muncul, berarti Admin API, S3 API
dengan SigV4, dan endpoint `/preview` semuanya bekerja.

Coba tarik file yang sama sekali lagi: kali ini statusnya
`sudah ada di bucket (isi identik)` — itu content-addressing bekerja.

**3. Cek dari sisi Garage**, di server:

```bash
curl -s -o /dev/null -w '%{http_code}\n' -H "Host: uji-panel" \
  http://127.0.0.1:3902/NAMA_FILE_HASIL_UPLOAD
```

`200` berarti routing lewat Host header sudah benar — persis yang nanti dilakukan
Caddy untuk domain sungguhan.

**4. Pasang domain** (butuh DNS yang sudah mengarah ke server ini). Di halaman
**Domains**, isi domain, pilih bucket `uji-panel`, klik **Tambah & reload Caddy**.
Harus muncul pesan hijau. Cek filenya di server:

```bash
cat /etc/caddy/sites/DOMAIN_KAMU.caddy
systemctl is-active caddy
```

Lalu buka `https://DOMAIN_KAMU/NAMA_FILE_HASIL_UPLOAD` di browser. Sertifikat TLS
diterbitkan Caddy otomatis, kadang butuh beberapa detik pada permintaan pertama.

**5. Bersihkan.** Hapus domainnya dulu (panel akan menolak menghapus bucket yang
masih punya domain — itu memang disengaja), lalu hapus objeknya, lalu hapus
bucket `uji-panel` dengan mengetik ulang namanya.

Terakhir, cek key sisa dari bucket uji coba — panel sengaja tidak menghapus key
saat bucket dihapus:

```bash
garage key list
garage key delete uji-panel-key     # tambahkan --yes kalau diminta konfirmasi
```

### Checklist instalasi

| # | Langkah | Verifikasi | Hasil |
|---|---|---|---|
| 0 | Garage & Caddy siap | `curl -H "Authorization: Bearer …" …/v2/GetClusterHealth` | JSON `status` |
| 1 | Build | `file garagepanel` | `ELF 64-bit … statically linked` |
| 2 | Binary terpasang | `/usr/local/bin/garagepanel` | pesan `GARAGE_ADMIN_TOKEN belum diisi` |
| 3 | User dibuat | `id garagepanel` | uid/gid tampil |
| 4 | Direktori sites | `stat -c '%U %G %a' /etc/caddy/sites` | `garagepanel caddy 2750` |
| 5 | Sudoers | `sudo -u garagepanel sudo -n /bin/systemctl reload caddy` | `sudoers OK` |
| 6 | Key S3 | `garage key list` | `garagepanel` ada |
| 7 | File env | `stat -c '%U %G %a' /etc/garagepanel/garagepanel.env` | `root root 600` |
| 8 | Service | `systemctl is-active garagepanel` | `active` |
| 9 | Tunnel | buka `http://127.0.0.1:8090` | halaman Buckets |
| 10 | Uji end-to-end | upload + thumbnail muncul | semua hijau |
| — | *(opsional)* [Login](#login-dan-akses-lewat-domain) | buka panel setelah restart | form login muncul |

## Login dan akses lewat domain

Secara bawaan panel tidak punya halaman login: ia hanya mendengarkan di loopback
dan diakses lewat SSH tunnel, jadi **SSH-nya sendiri yang jadi autentikasi**.
Untuk banyak pemakaian itu sudah cukup dan tidak perlu diubah.

Aktifkan login kalau kamu ingin:

- membuka panel lewat domain (`https://panel.domainmu.com`) tanpa tunnel, atau
- menambah lapisan password walau tetap lewat tunnel, misalnya karena server itu
  punya beberapa pemakai SSH.

### 1. Buat hash password

Password tidak pernah disimpan, yang disimpan hanya hash PBKDF2-HMAC-SHA256
(600.000 iterasi, sesuai anjuran OWASP). Perhitungannya memakai `crypto/pbkdf2`
dari pustaka standar Go — tidak ada dependency tambahan.

```bash
garagepanel -hash-password
```

Perintah ini meminta password dua kali dengan echo terminal dimatikan, lalu
mencetak satu baris siap tempel:

```
PANEL_PASSWORD_HASH=pbkdf2-sha256.600000.zngcBfqApRZC243u9QU6eQ.Ca7BZ1yR2Zeq…
```

Password minimal 12 karakter. Ia tidak pernah dilewatkan sebagai argumen, jadi
tidak muncul di `ps` maupun di riwayat shell. Untuk otomasi, password bisa
dikirim lewat stdin:

```bash
printf '%s\n' "$PASSWORD" | garagepanel -hash-password
```

Hash-nya sengaja memakai titik sebagai pemisah, bukan `$` seperti format PHC:
`$600000` akan hilang ditelan ekspansi variabel shell dan menghasilkan hash yang
rusak tanpa peringatan apa pun.

### 2. Isi environment

Tambahkan ke `/etc/garagepanel/garagepanel.env`:

```bash
PANEL_USERNAME=admin
PANEL_PASSWORD_HASH=pbkdf2-sha256.600000.…
```

Kalau panel mau diakses lewat domain, tambahkan juga:

```bash
PANEL_DOMAIN=panel.domainmu.com
```

```bash
sudo systemctl restart garagepanel
```

Login aktif begitu `PANEL_PASSWORD_HASH` terisi. Tanpa itu, panel berjalan
seperti semula.

> Panel **menolak start** kalau `PANEL_DOMAIN` diisi tapi
> `PANEL_PASSWORD_HASH` kosong. Melayani sebuah domain tanpa login berarti
> menyerahkan hak menulis config Caddy dan reload systemd kepada siapa pun yang
> tahu alamatnya.

### 3. Pasang Caddy di depan panel

`LISTEN` tetap `127.0.0.1:8090` — panel **tidak pernah** mengikat ke antarmuka
publik. Caddy yang menerima dari luar lalu meneruskannya ke loopback.

Tulis `/etc/caddy/sites/_panel.caddy` (awalan `_` supaya panel tidak menganggapnya
domain bucket):

```caddy
panel.domainmu.com {
	reverse_proxy 127.0.0.1:8090
}
```

```bash
sudo systemctl reload caddy
```

Caddy v2 sudah meneruskan header `Host` apa adanya dan menambahkan
`X-Forwarded-Proto` serta `X-Forwarded-For`, dan ketiganya memang yang dipakai
panel: `Host` dicocokkan dengan `PANEL_DOMAIN`, `X-Forwarded-Proto` menentukan
apakah cookie ditandai `Secure`, dan `X-Forwarded-For` dipakai membedakan
pelaku percobaan login.

**Sangat disarankan menambah batasan IP** kalau memang hanya kamu yang akan
mengaksesnya. Satu password saja yang berdiri antara internet dan hak istimewa
panel itu tipis:

```caddy
panel.domainmu.com {
	@luar not remote_ip 203.0.113.10 198.51.100.0/24
	respond @luar "Forbidden" 403

	reverse_proxy 127.0.0.1:8090
}
```

Akses lewat SSH tunnel tetap bisa dipakai bersamaan — panel menerima Host
loopback maupun `PANEL_DOMAIN`.

### Yang dilakukan dan tidak dilakukan login ini

| | |
|---|---|
| Hash password | PBKDF2-HMAC-SHA256, 600.000 iterasi, salt acak 16 byte, dibandingkan dalam waktu konstan |
| Session | token acak 32 byte di cookie `HttpOnly`; disimpan di memori, jadi **restart panel = semua sesi berakhir** |
| Masa berlaku | 12 jam sejak aktivitas terakhir, maksimal 7 hari sejak login |
| Cookie | `SameSite=Lax`, `Secure` otomatis saat diakses lewat HTTPS |
| Pembatas | 5 kali gagal dari alamat yang sama → ditahan 15 menit; verifikasi dijalankan satu per satu supaya banjir request tidak menghabiskan CPU |
| Tidak ada | multi-user, reset password lewat email, 2FA. Satu username, satu password |

Lupa password? Buat hash baru, ganti nilainya di env file, lalu restart:

```bash
garagepanel -hash-password
sudo nano /etc/garagepanel/garagepanel.env
sudo systemctl restart garagepanel
```

Mau memaksa semua orang logout? `sudo systemctl restart garagepanel` sudah cukup —
session hanya ada di memori.

## Update ke versi baru

Panel tidak punya state sendiri — tidak ada database, tidak ada file cache.
Semua state ada di Garage dan di file `.caddy`. Jadi update cukup mengganti
binary:

```bash
# di mesin lokal
cd s3caddy && git pull
CGO_ENABLED=0 go build -ldflags="-s -w" -o garagepanel .
scp garagepanel user@server:/tmp/garagepanel
```

```bash
# di server
sudo install -o root -g root -m 0755 /tmp/garagepanel /usr/local/bin/garagepanel
rm /tmp/garagepanel
sudo systemctl restart garagepanel
systemctl is-active garagepanel
```

File environment, file `.caddy`, bucket, dan key tidak tersentuh. Kalau unit
systemd-nya ikut berubah, salin ulang lalu `sudo systemctl daemon-reload`
sebelum restart.

> Kalau build gagal dengan `package crypto/pbkdf2 is not in std`, Go di mesin
> build masih di bawah 1.24. Perbarui lewat [Go (di mesin build)](#go-di-mesin-build).

Setelah restart, cek sekali bahwa panel benar-benar naik lagi — bukan gagal
start karena konfigurasi yang berubah:

```bash
systemctl is-active garagepanel && journalctl -u garagepanel -n 5 --no-pager
```

## Backup dan pemulihan

Panel tidak menyimpan state apa pun sendiri. Yang perlu di-backup hanya dua
tempat, dan keduanya kecil:

| Apa | Di mana | Isinya |
|---|---|---|
| Konfigurasi panel | `/etc/garagepanel/garagepanel.env` | token admin, key S3, hash password login |
| Pemetaan domain | `/etc/caddy/sites/*.caddy` | domain → bucket, whitelist IP S3 API |

```bash
sudo tar czf garagepanel-backup-$(date +%F).tar.gz \
  /etc/garagepanel/garagepanel.env \
  /etc/caddy/sites \
  /etc/sudoers.d/garagepanel \
  /etc/systemd/system/garagepanel.service
```

Arsip ini memuat token admin Garage dan secret key S3 dalam bentuk terbaca.
Simpan seperti kamu menyimpan kunci SSH — jangan ke object storage yang dikelola
panel ini sendiri.

Memulihkannya: pasang binary seperti di [Langkah 2](#langkah-2--kirim-binary-ke-server),
buat user dan direktori seperti [Langkah 3](#langkah-3--buat-user-garagepanel)
dan [Langkah 4](#langkah-4--siapkan-direktori-sites-caddy), bongkar arsipnya,
lalu:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now garagepanel
sudo systemctl reload caddy
```

**Bucket dan objeknya sendiri tidak ikut di sini** — itu ada di Garage, di atas
ZFS. Backup data object storage adalah urusan terpisah; lihat
[panduan backup Garage](https://garagehq.deuxfleurs.fr/documentation/operations/recovering/).

## Uninstall

```bash
sudo systemctl disable --now garagepanel
sudo rm /etc/systemd/system/garagepanel.service
sudo systemctl daemon-reload
sudo rm -rf /etc/garagepanel
sudo rm -f /usr/local/bin/garagepanel /etc/sudoers.d/garagepanel
sudo userdel garagepanel
```

Yang **tidak** ikut terhapus, dan itu memang disengaja:

- **File `.caddy` di `/etc/caddy/sites/`** — semua domain tetap melayani seperti
  biasa tanpa panel. Hapus manual kalau memang tidak dipakai lagi, lalu
  `sudo systemctl reload caddy`.
- **Bucket dan objek di Garage** — tidak tersentuh sama sekali.
- **Key S3** yang pernah dibuat panel — lihat `garage key list`, hapus dengan
  `garage key delete <nama>`.
- **Token admin** kalau kamu memakai Pilihan B di Langkah 0c — cabut lewat
  `garage admin-token` (lihat `garage admin-token --help` untuk subperintah
  `list` dan `delete`).

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
| `PANEL_PASSWORD_HASH` | — | Hash password login. Kosong → tidak ada login (lihat [Login](#login-dan-akses-lewat-domain)) |
| `PANEL_USERNAME` | `admin` | Username untuk login |
| `PANEL_DOMAIN` | — | Domain yang boleh dipakai mengakses panel lewat reverse proxy. Wajib disertai `PANEL_PASSWORD_HASH` |

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

**Secret.** Token admin, secret key, dan password login tidak pernah ditulis ke
log. Error dari transport pun dibersihkan dari URL lengkap sebelum ditampilkan.
Access log hanya mencatat method, path, status, durasi — tanpa query string dan
tanpa header. Percobaan login yang gagal dicatat beserta alamat asalnya, tapi
username yang dicoba sengaja tidak ikut: kalau seseorang salah mengetik password
ke kolom username, isinya akan mendarat di journal.

**Login** (opsional, lihat [Login dan akses lewat domain](#login-dan-akses-lewat-domain)).
Tanpa `PANEL_PASSWORD_HASH` tidak ada autentikasi user sama sekali: siapa pun
yang bisa membuka port loopback di server itu — yaitu siapa pun yang punya akses
SSH — punya kendali penuh atas panel. Itu memang modelnya kalau aksesnya lewat
tunnel; amankan akses SSH-nya. Begitu login diaktifkan, password disimpan
sebagai hash PBKDF2-HMAC-SHA256 600.000 iterasi, session ada di memori dengan
batas idle 12 jam, dan percobaan gagal dibatasi 5 kali per alamat.

**Yang tidak dilakukan panel ini:** tidak ada multi-user, tidak ada 2FA, tidak
ada reset password mandiri. Satu username, satu password, satu operator.

## Troubleshooting

**`GARAGE_ADMIN_TOKEN belum diisi`** — panel sengaja tidak mau start. Cek
`/etc/garagepanel/garagepanel.env` terbaca oleh systemd:
`systemctl show garagepanel -p Environment`.

**`token admin ditolak — periksa GARAGE_ADMIN_TOKEN`** — token tidak cocok
dengan `admin_token` di `/etc/garage.toml`. Garage perlu di-restart kalau
tokennya baru diubah. Uji tokennya langsung:
`curl -s -H "Authorization: Bearer TOKEN" http://127.0.0.1:3903/v2/GetClusterHealth`.

**Sebagian halaman jalan, satu operasi menjawab 403** — kalau kamu memakai token
ber-scope (Pilihan B di [Langkah 0c](#langkah-0--periksa-garage-dan-caddy)),
scope-nya kurang. Panel memakai delapan operasi:
`GetClusterHealth`, `ListBuckets`, `GetBucketInfo`, `CreateBucket`,
`DeleteBucket`, `UpdateBucket`, `CreateKey`, `AllowBucketKey`. Buat ulang
tokennya dengan daftar lengkap itu. Gejala yang sama muncul kalau token
ber-`--expires-in` sudah kedaluwarsa.

**Caddy menolak reload karena pola `import` tidak cocok** — `/etc/caddy/sites`
masih kosong. Isi placeholder:
`printf '# placeholder\n' | sudo tee /etc/caddy/sites/_placeholder.caddy`
lalu reload lagi. Nama berawalan `_` diabaikan panel.

**`Tidak bisa listen di 127.0.0.1:8090: address already in use`** — ada proses
lain di port itu, sering kali instance panel lama. Cek dengan
`sudo ss -lntp | grep 8090`, matikan, atau ganti `LISTEN` ke port lain.

**`PANEL_DOMAIN diisi tapi PANEL_PASSWORD_HASH kosong`** — panel sengaja menolak
start. Buat hash-nya (`garagepanel -hash-password`), atau kosongkan
`PANEL_DOMAIN` dan pakai SSH tunnel.

**`PANEL_PASSWORD_HASH tidak bisa dibaca`** — hash-nya rusak. Penyebab paling
sering: hash lama yang memakai `$` lalu dipotong ekspansi shell. Buat ulang
dengan `garagepanel -hash-password` dan tempel apa adanya, tanpa tanda kutip.

**Login selalu ditolak padahal password benar** — kalau baru saja 5 kali gagal,
alamatmu sedang ditahan 15 menit; penguncian berlaku juga untuk password yang
benar. Tunggu, atau `sudo systemctl restart garagepanel` untuk mengosongkan
catatannya.

**Halaman login muncul terus setelah berhasil masuk** — cookie session tidak
tersimpan. Kalau panel di belakang Caddy, pastikan diakses lewat `https://`:
saat `X-Forwarded-Proto: https` diteruskan, cookie ditandai `Secure` dan browser
tidak akan mengirimkannya kembali lewat `http://` biasa.

**`panel hanya melayani host loopback … atau panel.domainmu.com`** — Host yang
sampai ke panel bukan salah satu dari itu. Cocokkan alamat di blok Caddy dengan
nilai `PANEL_DOMAIN`; Caddy v2 meneruskan `Host` apa adanya, jadi keduanya harus
sama persis.

**Reload Caddy gagal, pesannya cuma "Job for caddy.service failed"** — panel
sudah mencoba membaca journal Caddy untuk menampilkan error aslinya. Kalau
bagian "journal caddy" kosong, tambahkan user ke grup journal:
`usermod -aG systemd-journal garagepanel && systemctl restart garagepanel`.

**`sudo: a password is required`** — path `systemctl` di sudoers tidak sama
dengan yang dipanggil panel. Bandingkan `command -v systemctl` dengan
`CADDY_RELOAD_CMD`.

**`tidak bisa menulis di /etc/caddy/sites`** — cek kepemilikan direktori:
`stat -c '%U %G %a' /etc/caddy/sites` harus `garagepanel caddy 2750`.

**Permission ditolak padahal kepemilikan sudah benar (Fedora/RHEL/Rocky)** —
SELinux kemungkinan memblokirnya. Periksa dulu apakah memang itu penyebabnya:

```bash
getenforce                                    # Enforcing?
sudo ausearch -m AVC -ts recent | grep -i garagepanel
```

Kalau ada baris AVC yang cocok, buat kebijakan khusus dari catatan itu — jangan
mematikan SELinux:

```bash
sudo ausearch -m AVC -ts recent | audit2allow -M garagepanel
sudo semodule -i garagepanel.pp
```

Di Debian/Ubuntu dengan AppArmor, profil bawaan Caddy tidak membatasi panel
(prosesnya terpisah), jadi biasanya bukan ini penyebabnya.

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
auth.go               hash password, session, pembatas percobaan login
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
go test -cover ./...   # ~76% statement coverage
```

Menjalankan panel lokal tanpa server Garage sungguhan: lihat
`newTestPanel` di `main_test.go` — isinya Garage + S3 + web endpoint tiruan yang
bisa dipakai ulang.
