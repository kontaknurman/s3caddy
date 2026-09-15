# Garage + Caddy Admin Panel

Panel admin untuk mengelola bucket [Garage](https://garagehq.deuxfleurs.fr/) S3
dan domain [Caddy](https://caddyserver.com/) di satu server Linux.

Go murni, **tanpa dependency eksternal** — tidak ada framework web, tidak ada
AWS SDK. Template di-embed dengan `//go:embed`, hasil build adalah satu binary
statis tanpa file pendukung. Satu-satunya program tambahan adalah
[rclone](https://rclone.org/), itu pun opsional dan hanya untuk fitur Sync
(impor/ekspor bucket dari Wasabi dan layanan S3 lain).

```
$ go list -m all
github.com/kontaknurman/s3caddy       # tidak ada baris lain
```

## Isi

- [Yang bisa dilakukan](#yang-bisa-dilakukan)
- [Cara kerja](#cara-kerja)
- [Peta komponen](#peta-komponen)
- [Memasang Go, Caddy, dan rclone](#memasang-go-caddy-dan-rclone)
  - [Go (di mesin build)](#go-di-mesin-build)
  - [Caddy (di server)](#caddy-di-server)
  - [rclone (di server, untuk Sync)](#rclone-di-server-untuk-sync)
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
- [Diverifikasi di Ubuntu 24.04](#diverifikasi-di-ubuntu-2404)
- [Login dan akses lewat domain](#login-dan-akses-lewat-domain)
- [Sync dengan S3 lain (Wasabi, dll.)](#sync-dengan-s3-lain-wasabi-dll)
  - [Menambah remote](#menambah-remote)
  - [Memulai impor atau ekspor](#memulai-impor-atau-ekspor)
  - [Jadwal: jalankan berulang](#jadwal-jalankan-berulang)
  - [Cara kerja job](#cara-kerja-job)
  - [Restart, jeda, dan key yang gagal](#restart-jeda-dan-key-yang-gagal)
  - [Biaya request dan batasan](#biaya-request-dan-batasan)
- [File manager di halaman Objek](#file-manager-di-halaman-objek)
- [Halaman Status](#halaman-status)
- [Update ke versi baru](#update-ke-versi-baru)
  - [1. Catat versi yang sedang berjalan](#1-catat-versi-yang-sedang-berjalan)
  - [2. Build versi baru](#2-build-versi-baru)
  - [3. Simpan binary lama, lalu pasang yang baru](#3-simpan-binary-lama-lalu-pasang-yang-baru)
  - [4. Pastikan updatenya benar-benar jalan](#4-pastikan-updatenya-benar-benar-jalan)
  - [Kalau ada yang tidak beres](#kalau-ada-yang-tidak-beres)
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
| **Objek** | File manager per bucket: breadcrumb, tampilan grid/daftar, urut nama/ukuran/tanggal, folder baru, upload drag-and-drop (nama dari isi file atau nama asli), ganti nama, pindah/salin antar bucket, pilih banyak → hapus, unduh, copy URL publik, operasi folder (salin/pindah/hapus) sebagai job latar |
| **Sync** | Impor/ekspor bucket dari/ke Wasabi, Amazon S3, Backblaze B2, Hetzner, Cloudflare R2, DigitalOcean, Scaleway, Ceph, MinIO, atau Garage lain. Mode "hanya yang belum ada", tahan ratusan juta objek, jalan berhari-hari di server, lanjut otomatis setelah restart, bisa dijadwalkan berulang (setiap N menit/jam/hari). Tes koneksi menguji baca **dan** tulis. Transfer dikerjakan rclone |
| **IP Whitelist** | Batasi operasi tulis ke S3 API per IP/CIDR; read (GET/HEAD) selalu terbuka |
| **Status** | Kondisi server: CPU per core, load, memori/swap, disk dan inode, I/O disk, jaringan, suhu, proses garage/caddy/rclone; kesehatan cluster Garage dan pemakaian disk tiap node. Diperbarui tiap 5 detik |

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
diuji terhadap tiga test vector resmi AWS (lihat `s3_test.go` dan
`s3_ops_test.go`).

**Sync = panel yang memutuskan, rclone yang menyalin.** Panel men-list bucket
sumber dan tujuan halaman demi halaman (1000 key per request, selalu terurut)
dan membandingkannya seperti merge sort — dua request listing per 1000 objek,
tanpa HEAD per objek, tanpa menahan daftar di memori. Key yang harus disalin
ditulis ke file chunk (maksimal 10.000 key) dan diserahkan ke
`rclone copy --files-from-raw`, yang menyalin persis daftar itu tanpa men-list
apa pun. Setiap chunk selesai, key terakhirnya dicatat sebagai checkpoint di
`/var/lib/garagepanel`. Detailnya di [Sync dengan S3 lain](#sync-dengan-s3-lain-wasabi-dll).

**State yang ada hanya untuk Sync.** Selain file `.caddy`, panel menulis ke
`/var/lib/garagepanel`: daftar remote (dengan secret-nya, mode 0600) dan
checkpoint job. Itu bukan database: isi bucket tetap satu-satunya sumber
kebenaran, dan panel tanpa direktori ini tetap berjalan — hanya halaman Sync
yang nonaktif.

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
                                               ├──▶ tulis /etc/caddy/sites/*.caddy
                                               │       lalu `systemctl reload caddy`
                                               │
                                               └──▶ rclone (proses anak, user garagepanel)
                                                       Garage S3 ◀──▶ Wasabi / AWS / MinIO
                                                       daftar key dari panel, kredensial lewat env
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
| rclone 1.59+ | — | ✔ hanya kalau fitur Sync dipakai |
| garagepanel | dibuat di sini | ✔ dijalankan di sini |

## Memasang Go, Caddy, dan rclone

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

### rclone (di server, untuk Sync)

Hanya perlu kalau kamu memakai halaman Sync. Panel tidak butuh `rclone config`
sama sekali: kredensial diberikan lewat environment proses anak, per job.

Ubuntu 24.04 menyediakan versi yang cukup (1.60):

```bash
sudo apt install rclone
rclone version        # harus: rclone v1.59 atau lebih baru
```

Distro yang paketnya lebih tua dari 1.59 (Ubuntu 22.04 punya 1.53 — terlalu
lama, panel menolaknya karena butuh `--metadata`): pakai skrip resmi, yang
memasang binary statis ke `/usr/bin/rclone`:

```bash
curl -fsSL https://rclone.org/install.sh | sudo bash
rclone version
```

rclone berjalan sebagai user `garagepanel`, tanpa sudo, di dalam sandbox unit
systemd yang sama. Tidak ada yang perlu dikonfigurasi selain memasangnya. Kalau
dipasang belakangan, `sudo systemctl restart garagepanel` supaya panel
mendeteksinya.

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
| rclone 1.59+ di server (opsional, untuk Sync) | `rclone version` | `rclone v1.59` atau lebih baru |

Belum ada Go atau Caddy? Lihat [Memasang Go dan Caddy](#memasang-go-caddy-dan-rclone)
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
dicabut tanpa mengganggu yang lain. Panel hanya memakai sembilan operasi:

```bash
garage admin-token create --expires-in 365d \
  --scope GetClusterHealth,GetClusterStatus,ListBuckets,GetBucketInfo,CreateBucket,DeleteBucket,UpdateBucket,CreateKey,AllowBucketKey \
  garagepanel
```

`GetClusterStatus` hanya dipakai halaman Status (daftar node dan pemakaian
disk Garage); tanpa scope itu halaman lain tetap jalan dan kartu Garage di
halaman Status menjelaskan scope yang kurang.

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

> **Di Ubuntu, `/bin/systemctl` sudah benar** walaupun `command -v systemctl`
> menjawab `/usr/bin/systemctl`: `/bin` adalah symlink ke `/usr/bin` dan sudo
> mencocokkannya. Sudah diuji di Ubuntu 24.04 — lihat
> [Diverifikasi di Ubuntu 24.04](#diverifikasi-di-ubuntu-2404).

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

Baris `StateDirectory=garagepanel` di unit membuat `/var/lib/garagepanel`
(milik `garagepanel`, mode 0700) secara otomatis — tidak ada `mkdir` manual.
Di sanalah daftar remote dan checkpoint job Sync disimpan:

```bash
stat -c '%U %G %a' /var/lib/garagepanel     # harus: garagepanel garagepanel 700
```

Di log start harus ada satu dari dua baris ini — keduanya normal:

```
garagepanel: sync aktif: rclone v1.60.1, state di /var/lib/garagepanel, 0 remote, 0 job tersimpan
garagepanel: PERINGATAN: rclone tidak ditemukan ("rclone"): ... Pasang dengan: sudo apt install rclone
```

Yang kedua hanya berarti halaman Sync nonaktif sampai rclone dipasang; halaman
lain tidak terpengaruh.

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

## Diverifikasi di Ubuntu 24.04

Seluruh prosedur di atas dijalankan di Ubuntu 24.04.4 LTS dengan systemd 255 dan
Caddy 2.11.4 dari repositori resmi. Yang diuji bukan sekadar "perintahnya jalan",
tapi klaim-klaim yang jadi dasar desainnya:

| Yang diklaim | Cara membuktikan | Hasil |
|---|---|---|
| Caddyfile buatan panel sah | `caddy validate` atas config lengkap termasuk `import` | `Valid configuration` |
| `header_up Host <bucket>` benar-benar mengganti Host | upstream tiruan menggemakan header yang diterima | `Host diterima upstream: media` |
| Whitelist membuka baca, menutup tulis | request dari IP tak terdaftar | GET/HEAD `200`; PUT/POST/DELETE/PATCH `403` |
| Whitelist meloloskan IP terdaftar | IP dimasukkan ke daftar | PUT/DELETE `200` |
| Caddy bisa membaca file buatan panel | `sudo -u caddy caddy validate` | `Valid configuration` |
| Caddy **tidak** bisa menulis di sana | `sudo -u caddy` menulis file | `Permission denied` |
| Reload gagal terdeteksi | file site sengaja dirusak lalu reload | exit `1` + nama file dan nomor baris |
| Rollback bekerja | reload dipaksa gagal saat menambah domain | file panel terhapus lagi, pesan Caddy tampil di UI |
| sudoers hanya mengizinkan satu perintah | `sudo -n -l` untuk perintah lain | `restart caddy` dan perintah lain ditolak |
| Unit systemd sah | `systemd-analyze verify` | tanpa keluhan |

### Hal khas Ubuntu

**`systemctl` ada di `/usr/bin`, bukan `/bin`.** Default panel memanggil
`/bin/systemctl`, dan itu **tetap benar di Ubuntu**: `/bin` adalah symlink ke
`/usr/bin` (usrmerge), dan sudo mencocokkannya. Sudah diuji dua arah — entri
sudoers `/bin/systemctl` mengizinkan pemanggilan lewat `/bin` maupun `/usr/bin`,
sementara `systemctl restart caddy` tetap ditolak. Jadi baris sudoers di
[Langkah 5](#langkah-5--pasang-sudoers) bisa dipakai apa adanya.

**AppArmor bukan penghalang.** Paket Caddy dari repositori resmi tidak memasang
profil AppArmor sama sekali (`dpkg -L caddy | grep apparmor` kosong), dan tidak
ada profil untuk panel. Bagian SELinux di [Troubleshooting](#troubleshooting)
hanya berlaku untuk Fedora/RHEL — Ubuntu tidak memakainya.

**Paket Caddy memakai `ExecReload=caddy reload --config … --force`,** yang
memvalidasi config dan mengembalikan exit code bukan 0 kalau gagal. Itulah yang
membuat rollback panel bisa bekerja; sudah dibuktikan dengan merusak satu file
site dan memeriksa exit code-nya.

**`ss`** (dipakai di satu perintah troubleshooting) berasal dari paket
`iproute2`, yang biasanya sudah ada di instalasi Ubuntu Server. Kalau tidak:
`sudo apt install iproute2`.

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

> **`PANEL_DOMAIN` dan `S3_API_DOMAIN` itu dua hal berbeda.** Yang pertama untuk
> membuka UI panel di browser; yang kedua untuk aplikasimu berbicara ke S3 API.
> Mengisi `PANEL_DOMAIN` saja tidak membuat endpoint S3 bisa diakses dari luar —
> kartu kredensial akan tetap menampilkan `http://127.0.0.1:3900`, dan memang
> begitu adanya sampai `S3_API_DOMAIN` diisi dan S3 API diekspos lewat halaman
> IP Whitelist. Keduanya boleh dipasang bersamaan di satu server.

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

## Sync dengan S3 lain (Wasabi, dll.)

Halaman **Sync** menyalin isi satu bucket dari layanan S3 lain ke Garage
(impor) atau sebaliknya (ekspor). Dirancang untuk bucket dengan ratusan juta
objek dan proses yang memakan hari: job jalan di server, bukan di browser, dan
dilanjutkan otomatis dari checkpoint setelah panel di-restart atau server
reboot.

Prasyarat: rclone terpasang di server (lihat
[rclone (di server, untuk Sync)](#rclone-di-server-untuk-sync)) dan
`/var/lib/garagepanel` ada (dibuat unit systemd). Kalau salah satunya kurang,
halaman Sync menampilkan alasannya beserta perintah perbaikannya.

### Menambah remote

Remote = satu endpoint S3 beserta kredensialnya. Klik **+ Tambah remote** di
kartu **Remote S3**. Memilih provider mengisi contoh endpoint dan region serta
menampilkan catatan khas penyedianya:

| Provider | Endpoint | Region | Catatan |
|---|---|---|---|
| Wasabi | `https://s3.wasabisys.com` (us-east-1) atau `https://s3.<region>.wasabisys.com` | `us-east-1`, `ap-southeast-1`, `eu-central-1`, … | region harus cocok dengan endpoint |
| Amazon S3 | `https://s3.<region>.amazonaws.com` | region bucket | IAM user dengan `s3:ListBucket`, `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` |
| Backblaze B2 | `https://s3.<region>.backblazeb2.com` — tertulis di halaman bucket B2 | `<region>` dari endpoint, misalnya `us-west-004` | access key = **keyID** dari *application key* yang dibuat sendiri di menu Application Keys. **Master application key tidak bisa dipakai di S3 API** — errornya `InvalidAccessKeyId: Malformed Access Key Id` |
| Hetzner Object Storage | `https://<lokasi>.your-objectstorage.com` | lokasi: `fsn1`, `nbg1`, `hel1` | hanya melayani gaya alamat **virtual-host** (`bucket.fsn1.your-objectstorage.com`); kredensial berlaku per project, jadi bucket dan key harus dari project yang sama |
| Cloudflare R2 | `https://<account-id>.r2.cloudflarestorage.com` | `auto` | token API R2 dengan izin Object Read & Write |
| DigitalOcean Spaces | `https://<dc>.digitaloceanspaces.com` | `sgp1`, `nyc3`, `ams3`, … | |
| Scaleway | `https://s3.<region>.scw.cloud` | `fr-par`, `nl-ams`, `pl-waw` | |
| Ceph RGW | alamat servernya | nama zonegroup, sering `default` | kalau server hanya menerima virtual-host, biarkan gaya alamat otomatis |
| MinIO | alamat servernya, boleh `http://` di LAN | `us-east-1` kecuali diubah di server | |
| Lainnya (Garage lain, dll.) | alamat servernya | Garage: `s3_region` di `garage.toml`-nya | |

Kolom lain:

| Kolom | Isi |
|---|---|
| Nama | pengenal pendek, huruf kecil/angka/tanda hubung, misalnya `wasabi-sg`; tidak bisa diubah setelah dibuat |
| Endpoint | skema + host saja, tanpa path. `http://` boleh (LAN) — panel menandainya "tanpa TLS" karena isi objek lewat tanpa enkripsi; secret sendiri tidak pernah dikirim, SigV4 hanya mengirim tanda tangannya |
| Region | region untuk tanda tangan SigV4; **harus cocok dengan endpoint** |
| Access key / Secret key | dari penyedia. Spasi atau baris baru di ujung dipangkas otomatis |
| Gaya alamat | `Otomatis` (bawaan), `Path-style` (`https://host/bucket/key`), atau `Virtual-host` (`https://bucket.host/key`) — lihat di bawah |

**Gaya alamat.** Setiap request S3 menyebut bucket entah di path
(`host/bucket/key`, *path-style*) atau di nama host (`bucket.host/key`,
*virtual-host*). Kebanyakan penyedia menerima keduanya, tapi tidak semua:
Hetzner dan sebagian Ceph hanya menerima virtual-host — dengan path-style,
listing kadang masih jalan tapi `PutObject` dijawab `403 AccessDenied`. Pada
setelan `Otomatis` panel memakai gaya bawaan provider itu; kalau ditolak, gaya
satunya dicoba, dan gaya yang diterima disimpan di remote lalu diteruskan ke
rclone (`force_path_style`). Kartu remote menampilkan gaya yang sedang dipakai.

**Tes koneksi.** Isi nama bucket lalu klik **Tes koneksi**: panel membaca satu
key di bucket itu, lalu — kecuali kamu memilih "hanya baca" — menulis satu
objek uji `.garagepanel-probe-<acak>` dan menghapusnya lagi. Kalau tesnya lulus,
endpoint, region, kredensial, izin tulis, dan gaya alamat semuanya sudah benar.
Pesan error yang muncul adalah pesan asli penyedianya, ditambah petunjuk
penyebab yang lazim untuk provider itu (lihat
[Troubleshooting](#troubleshooting)).

**Edit.** Tombol **Edit** di kartu remote membuka form yang sama. Kolom access
key dan secret key boleh dikosongkan — artinya tetap memakai yang tersimpan;
kalau access key diganti, secret-nya harus diisi juga. Remote yang sedang
dipakai job berjalan tidak bisa diubah atau dihapus; remote yang dipakai sebuah
jadwal tidak bisa dihapus sebelum jadwalnya dihapus.

Kredensial disimpan di `/var/lib/garagepanel/remotes.json` mode 0600 dan tidak
pernah ditampilkan lagi (access key disamarkan, secret tidak pernah dirender).

### Memulai impor atau ekspor

Di kartu **Impor / ekspor bucket**:

1. **Arah**: Impor (remote → Garage) atau Ekspor (Garage → remote). Panel
   sumber selalu tampil di kiri, tujuan di kanan.
2. Remote, bucket di remote, bucket di Garage, dan folder opsional di
   masing-masing sisi (`foto/2026/`; `/` di ujung ditambahkan otomatis).
   Folder sumber dan tujuan boleh beda: `wasabi:backup/2024/` →
   `garage:arsip/`.
3. **Objek yang sudah ada di tujuan**:
   - **Lewati** (bawaan) — hanya salin yang belum ada. Ini mode "update saja":
     jalankan berulang, hanya objek baru yang disalin.
   - **Perbarui kalau beda** — salin juga yang ukurannya beda di kedua sisi.
   - **Timpa semua** — salin semua tanpa memeriksa tujuan (tujuan tidak
     di-list sama sekali).
4. **Transfer paralel** (1–16, bawaan 4): jumlah objek yang disalin
   bersamaan oleh rclone.
5. **Mulai job sekarang** — atau buka *"…atau jalankan berulang sesuai
   jadwal"* untuk menyimpan setelan yang sama sebagai
   [jadwal](#jadwal-jalankan-berulang).

Sebelum job dibuat, panel memeriksa kedua sisi: membaca satu key di sumber,
lalu di tujuan membaca **dan** menulis-hapus satu objek uji
(`.garagepanel-probe-<acak>`). Kalau salah satunya gagal, job tidak dibuat dan
pesan asli penyedia ditampilkan — lebih baik tahu sekarang daripada setelah
ribuan `PutObject` pertama ditolak. Setelah itu tab boleh ditutup. Angka di
kartu job diperbarui tiap 3 detik selama ada job berjalan; tab **Semua /
Berjalan / Selesai / Gagal** menyaring daftarnya.

Batas: satu job belum-selesai per bucket, dan maksimal 2 job berjalan
bersamaan (memori rclone).

### Jadwal: jalankan berulang

Untuk sinkronisasi rutin ("cronjob"): isi form job seperti biasa, buka
**…atau jalankan berulang sesuai jadwal**, isi intervalnya, lalu klik
**Simpan jadwal**.

| Kolom | Isi |
|---|---|
| Setiap | `30m`, `6h`, `1d`, `7d` — atau angka polos = jam. Minimal 15 menit, maksimal 30 hari |
| Run pertama | opsional; kosong = sekarang + interval. Run berikutnya = run sebelumnya + interval (bukan "selesai + interval"), jadi jam mulainya tetap |

Tiap run membuat job biasa dengan setelan yang tersimpan di jadwal (arah,
bucket, folder, mode, transfer). Job-nya muncul di daftar job dan bisa dijeda
atau dibatalkan seperti yang lain. Dengan mode **Lewati**, run rutin hanya
menyalin objek yang baru muncul sejak run sebelumnya.

Aturannya:

- Panel memeriksa jadwal tiap 30 detik. Kalau saat jatuh tempo bucket-nya
  masih dipakai job lain (misalnya run sebelumnya belum selesai), sudah ada 2
  job berjalan, atau tes koneksinya gagal, run itu **dilewati** dan alasannya
  dicatat di kolom "Run terakhir"; jadwal tetap maju ke waktu berikutnya.
- Run yang terlewat saat panel mati **tidak dirapel**: setelah start, run
  berikutnya jatuh pada kelipatan interval pertama setelah sekarang.
- **Jalankan sekarang** membuat job seketika tanpa menggeser jadwal.
  **Jeda** menahan run baru; **Aktifkan** menjadwalkan ulang dari sekarang +
  interval.
- Jadwal disimpan di `/var/lib/garagepanel/schedules.json` (0600) bersama 20
  riwayat run terakhirnya. Menghapus jadwal tidak menghapus job yang sudah
  dibuatnya; menghapus remote yang dipakai jadwal ditolak.
- Jadwal tidak memakai cron sistem: hanya jalan selama `garagepanel` jalan.

### Cara kerja job

```
  panel                                          rclone (proses anak)
  ─────                                          ────────────────────
  list sumber  ─┐  merge-join, 1000 key/halaman
  list tujuan  ─┘  → key yang harus disalin
  tulis chunk.txt (≤ 10.000 key relatif)  ───▶  rclone copy --files-from-raw chunk.txt
                                                 --no-traverse --no-check-dest -M ...
  baca log JSON (progres, key gagal)      ◀───  src:bucket/prefix/  dst:bucket/prefix/
  chunk selesai → checkpoint = key terakhir
  ulangi sampai sumber habis
```

Perintah rclone persisnya (bisa direproduksi manual untuk debugging):

```
rclone copy --files-from-raw <chunk> --no-traverse --use-json-log --stats 5s \
  --stats-log-level NOTICE --log-level NOTICE --transfers <N> --checkers <N> \
  --retries 3 --low-level-retries 10 --config "" --no-check-dest -M \
  --s3-upload-cutoff 8M --s3-chunk-size 8M --s3-upload-concurrency 2 \
  --cache-dir /var/lib/garagepanel/cache src:<bucket>/<prefix> dst:<bucket>/<prefix>
```

Remote `src` dan `dst` didefinisikan lewat environment proses anak
(`RCLONE_CONFIG_SRC_TYPE=s3`, `..._PROVIDER`, `..._ENDPOINT`, `..._REGION`,
`..._FORCE_PATH_STYLE` sesuai gaya alamat remote, `..._ACCESS_KEY_ID`,
`..._SECRET_ACCESS_KEY`, dst.) — tidak lewat argumen (terlihat di `ps`), tidak
lewat file. Environment itu dibangun dari nol: `GARAGE_ADMIN_TOKEN` dan
`PANEL_PASSWORD_HASH` milik panel tidak diwariskan ke rclone.

Kenapa tidak `rclone copy` polos? rclone menahan seluruh isi satu direktori di
memori (~1 KB per objek). Bucket yang dibuat panel ini justru datar — semua
objek di akar bucket — sehingga 100 juta objek berarti ±100 GB RAM. Dengan
`--files-from-raw --no-traverse`, rclone tidak men-list apa pun: ia hanya
mengakses key yang diberikan, satu per satu. Memori panel tetap satu halaman
listing per sisi; memori rclone ≈ `transfers × 2 × 8 MiB` untuk buffer
multipart plus ~100 MB dasar.

`-M` membawa metadata: Content-Type, Cache-Control, Content-Disposition,
Content-Encoding, dan `x-amz-meta-*` ikut menyeberang. Objek di atas 8 MiB
diunggah multipart dengan part 8 MiB, jadi tidak ada objek yang pernah disangga
utuh.

### Restart, jeda, dan key yang gagal

**Restart / reboot.** Job yang berstatus *berjalan* dilanjutkan otomatis saat
panel start, dari checkpoint chunk terakhir: kedua sisi di-list lagi dengan
`start-after=<checkpoint>`. Objek di chunk yang terputus diperiksa ulang — di
mode "Lewati" yang sudah tersalin terlihat ada di tujuan dan dilewati; di mode
"Timpa semua" disalin lagi. Angka di tabel bisa terhitung dua kali sebanyak
satu chunk.

**Jeda / Lanjutkan / Batalkan.** Jeda menulis niatnya ke disk dulu, baru
menghentikan rclone (SIGINT, rclone membatalkan multipart yang tergantung), jadi
crash di tengah "menjeda" tidak menghidupkan job lagi. Lanjutkan mulai dari
checkpoint. Batalkan menghentikan job untuk selamanya; objek yang sudah
tersalin tetap ada — **panel tidak pernah menghapus apa pun di tujuan**.

**Key yang gagal.** Objek yang gagal disalin (izin, key aneh, dsb.) dicatat ke
`/var/lib/garagepanel/jobs/<id>/failed.jsonl` beserta pesan errornya, dan job
lanjut ke key berikutnya. Tautan "lihat key yang gagal" menampilkan 500
terakhir; bisa diunduh seluruhnya. Setelah penyebabnya dibereskan, jalankan job
lagi dengan mode "Lewati": yang sudah tersalin dilewati, yang gagal dicoba lagi.

**Jeda otomatis.** Kalau di satu chunk paling sedikit 10 key gagal dan itu
separuh chunk atau lebih, panel menganggap salah satu sisi sedang tidak bisa
dihubungi: job dijeda, chunk itu **tidak** dicatat selesai dan key-nya tidak
dicatat gagal, jadi cukup klik Lanjutkan setelah koneksinya pulih. Job juga
dijeda setelah 1.000.000 key gagal dicatat.

**rclone keluar dengan error** (exit code 1/2/3/4/7 — argumen, remote tidak
ditemukan, fatal): job berstatus gagal dengan 20 baris log rclone terakhir.
Chunk itu tidak dicatat selesai; setelah penyebabnya diperbaiki, Lanjutkan.

### Biaya request dan batasan

Per 1000 objek yang diperiksa: 1 request listing di sumber + 1 di tujuan (tidak
ada listing tujuan di mode "Timpa semua"). Per objek yang disalin: 1 HEAD + 1
GET di sumber (rclone memeriksa objek yang diberi nama satu per satu), 1 PUT
(atau multipart) + 1 HEAD di tujuan. Wasabi tidak menagih request maupun egress,
tapi punya minimum retensi; AWS menagih keduanya — hitung dulu untuk bucket
besar.

Yang tidak dilakukan:

- Tidak menghapus di tujuan (bukan mirror). Objek yang hanya ada di tujuan
  dibiarkan.
- Tidak membandingkan ETag/checksum; "beda" berarti ukuran beda.
- Key yang memuat baris baru, `//`, atau segmen `.`/`..` tidak bisa
  diserahkan ke rclone lewat file daftar; dicatat sebagai gagal beserta
  alasannya.
- Folder kosong (objek marker `nama/` tanpa isi) ikut dibuat di tujuan, tapi
  ACL dan properti lain di luar metadata objek tidak dibawa.
- Sisa upload multipart yang tergantung setelah crash bisa dibersihkan di
  Garage dengan `garage bucket cleanup-incomplete-uploads`.

Perkiraan waktu: yang menentukan hampir selalu bandwidth dan latensi ke
penyedia, bukan panel. Sebagai gambaran, 100 juta objek kecil dengan 8 transfer
paralel pada latensi 50 ms ke Wasabi berarti berhari-hari — itulah kenapa job
dibuat tahan restart. Lihat laju di kolom progres, dan batasi kalau perlu:
`RCLONE_EXTRA_ARGS="--bwlimit 20M"` di file environment.

Garage di belakang Cloudflare menolak tanda tangan rclone? Tambahkan
`RCLONE_EXTRA_ARGS="--s3-sign-accept-encoding=false"`.

## File manager di halaman Objek

Halaman Objek adalah file manager per bucket:

- **Folder** = prefix key yang diakhiri `/`. "Folder baru" menulis objek
  marker kosong `prefix/nama/` (konvensi yang sama dengan S3 Console dan
  rclone), breadcrumb di atas daftar menunjukkan posisi.
- **Tampilan** grid (thumbnail untuk gambar, kotak ekstensi untuk file lain)
  atau daftar (tabel), diingat per browser lewat cookie. **Urutan** nama,
  ukuran, atau tanggal — urutan ukuran/tanggal hanya berlaku di dalam halaman
  yang sedang tampil, karena S3 selalu mengembalikan 100 objek per halaman
  terurut nama.
- **Upload** masuk ke folder yang sedang dibuka, dengan dua mode penamaan yang
  dipilih saat upload: **nama dari isi file** (`sha256(isi)[:16] + ekstensi`,
  anti-duplikat, seperti sebelumnya) atau **nama asli file** (hanya nama
  dasarnya; direktori dari client dibuang). Di mode nama asli, file yang sudah
  ada tidak ditimpa kecuali kotak "timpa yang sudah ada" dicentang. Whitelist
  ekstensi dan batas 50 MB berlaku di kedua mode.
- **Ganti nama / pindah / salin** satu objek lewat menu `⋯`. S3 tidak punya
  operasi rename, jadi ini `CopyObject` (server-side, metadata ikut) lalu
  `DeleteObject`; kalau langkah hapusnya gagal, pesannya mengatakan objek sudah
  tersalin tapi yang lama masih ada. Tujuan yang sudah ada tidak ditimpa
  tanpa centang "timpa". Ekstensi boleh dipertahankan, tapi ekstensi baru
  harus dari whitelist upload.
- **Pilih banyak → Hapus terpilih**: satu request `DeleteObjects` untuk maksimal
  1000 objek; yang gagal disebutkan satu per satu.
- **Unduh** memaksa `Content-Disposition: attachment` untuk semua tipe.
- **Operasi folder** (salin / pindah / hapus folder yang sedang dibuka beserta
  isinya) dijalankan sebagai job latar di halaman Sync, karena satu folder bisa
  berisi jutaan objek. Hapus folder minta nama foldernya diketik ulang. Salin
  dan pindah memakai `CopyObject` server-side di dalam Garage (rclone dengan
  remote yang sama di kedua sisi), jadi datanya tidak keluar-masuk server.

## Halaman Status

Halaman **Status** menampilkan kondisi server dan cluster tanpa perlu SSH:

| Kartu | Isi | Sumber |
|---|---|---|
| CPU | pemakaian total dan per core, rincian user/system/menunggu I/O/steal, load average 1/5/15 menit, jumlah proses, tekanan PSI | `/proc/stat`, `/proc/loadavg`, `/proc/pressure/*` |
| Memori | dipakai (total − tersedia), tersedia, bebas, cache/buffer, shared, swap | `/proc/meminfo` |
| Disk | tiap filesystem nyata (ext4, xfs, zfs, btrfs, nfs, …): terpakai/total seperti `df`, sisa, inode | `/proc/mounts` + `statfs` |
| Jaringan | laju ↓/↑ per antarmuka sejak pembaruan terakhir, total sejak boot, error/drop | `/proc/net/dev` |
| Disk I/O | baca/tulis per perangkat blok, IOPS, persentase sibuk | `/proc/diskstats` |
| Suhu | sensor hwmon (coretemp, k10temp, nvme, …) atau thermal zone | `/sys/class/hwmon`, `/sys/class/thermal` |
| Proses penting | garage, caddy, garagepanel, rclone: PID, status, CPU %, RSS, thread, sejak kapan | `/proc/<pid>/{comm,stat,status}` |
| Garage | kesehatan cluster (node terhubung, node storage aktif, partisi kuorum/lengkap) dan tiap node: zona, kapasitas, status, pemakaian disk data dan metadata, versi | Admin API `GetClusterHealth`, `GetClusterStatus` |
| Panel ini | versi, Go, uptime, goroutine/heap, ringkasan Sync | proses panel sendiri |

Angka diperbarui tiap 5 detik selama tab terbuka (checkbox di atas
mematikannya; tab yang tidak terlihat juga berhenti memuat). Laju dan
persentase CPU adalah rata-rata sejak pembaruan sebelumnya. Warna: hijau
normal; kuning perlu diperhatikan (CPU ≥ 70 %, memori ≥ 80 %, disk ≥ 80 %,
suhu ≥ 70 °C, swap ≥ 30 %); merah kritis (CPU ≥ 90 %, memori ≥ 95 %, disk
≥ 90 %, inode ≥ 95 %, suhu ≥ 85 °C, swap ≥ 70 %). Load average dibandingkan
dengan jumlah core.

Semuanya dibaca dari `/proc` dan `/sys` sebagai user `garagepanel`: tidak ada
perintah yang dijalankan, tidak ada hak tambahan, dan unit systemd tidak perlu
diubah. Yang perlu diketahui:

- **Daftar node Garage** memakai `GetClusterStatus`. Token ber-scope
  (Pilihan B di Langkah 0c) harus memuat scope itu; kalau tidak, kartu Garage
  tetap menampilkan kesehatan cluster dan menyebutkan scope yang kurang.
- **Proses garage dan caddy** hanya terlihat kalau `/proc` tidak dipasang
  dengan `hidepid=`. Kalau dipasang, halaman menampilkan peringatan; proses
  panel sendiri tetap terlihat.
- **Suhu** hanya ada di server fisik dengan modul sensor yang dimuat
  (`coretemp`, `k10temp`, NVMe). Di VPS kartunya kosong, dan itu normal.
- **Mount jaringan yang macet** (NFS/CIFS) tidak menggantung halaman: `statfs`
  dibatasi 2 detik dan mount itu dilaporkan "tidak menjawab".
- Memori "dipakai" mengikuti definisi `free`: total dikurangi `MemAvailable`,
  jadi page cache tidak dihitung — kernel melepasnya kapan saja.

## Update ke versi baru

State panel kecil dan tidak butuh migrasi: file `.caddy` di `/etc/caddy/sites`
dan, sejak fitur Sync, `/var/lib/garagepanel` (remote + checkpoint job). Update
berarti mengganti satu file binary — dan sekali ini, menyalin ulang unit
systemd (lihat [di bawah](#kalau-unit-systemd-ikut-berubah)).

**File environment, file `.caddy`, `/var/lib/garagepanel`, bucket, objek, dan
key tidak tersentuh.** Job Sync yang sedang berjalan dilanjutkan dari
checkpoint setelah restart.

### 1. Catat versi yang sedang berjalan

Supaya nanti bisa dipastikan updatenya benar-benar terpasang:

```bash
garagepanel -version
```

Contoh keluaran: `9c9c631260fe (2026-09-14 07:26)` — itu commit git beserta
tanggalnya. Versi yang sama juga tercetak di log saat start dan di pojok bawah
setiap halaman panel.

### 2. Build versi baru

Di mesin build:

```bash
cd s3caddy
git pull
CGO_ENABLED=0 go build -ldflags="-s -w" -o garagepanel .
./garagepanel -version      # harus beda dari langkah 1
```

Go menyetempel revisi git ke dalam binary secara otomatis, jadi tidak perlu
flag khusus. Kalau muncul `-dirty`, berarti ada perubahan yang belum di-commit
di direktori kerjamu.

> Build gagal dengan `package crypto/pbkdf2 is not in std`? Go di mesin build
> masih di bawah 1.24 — perbarui lewat [Go (di mesin build)](#go-di-mesin-build).

```bash
scp garagepanel user@server:/tmp/garagepanel
```

### 3. Simpan binary lama, lalu pasang yang baru

Menyimpan yang lama membuat rollback jadi satu perintah kalau ada yang tidak
beres:

```bash
sudo cp /usr/local/bin/garagepanel /usr/local/bin/garagepanel.bak
sudo install -o root -g root -m 0755 /tmp/garagepanel /usr/local/bin/garagepanel
rm /tmp/garagepanel
sudo systemctl restart garagepanel
```

### 4. Pastikan updatenya benar-benar jalan

```bash
garagepanel -version                        # harus cocok dengan langkah 2
systemctl is-active garagepanel             # active
journalctl -u garagepanel -n 15 --no-pager
```

Di log harus terlihat baris versi dan baris siap:

```
garagepanel: versi 9c9c631260fe (2026-09-14 07:26)
garagepanel: terhubung ke Garage Admin API di http://127.0.0.1:3903
garagepanel: siap di http://127.0.0.1:8090 (loopback saja — akses lewat SSH tunnel)
```

Terakhir, buka panelnya dan pastikan halaman Buckets memuat daftar seperti
biasa. Versi yang sedang berjalan tertulis di pojok bawah halaman, jadi bisa
dicek dari browser tanpa SSH.

### Kalau ada yang tidak beres

Kembali ke binary sebelumnya:

```bash
sudo cp /usr/local/bin/garagepanel.bak /usr/local/bin/garagepanel
sudo systemctl restart garagepanel
garagepanel -version
```

Rollback ini aman karena tidak ada migrasi data sama sekali — versi lama
membaca file `.caddy` dan file environment yang sama persis.

### Kalau unit systemd ikut berubah

`garagepanel.service` jarang berubah, tapi kalau iya, salin ulang dan muat
ulang systemd sebelum restart. **Versi dengan fitur Sync mengubahnya**: ada
baris `StateDirectory=garagepanel` (membuat `/var/lib/garagepanel`),
`MemoryMax=1G`, dan `TimeoutStopSec=30`. Tanpa unit baru, halaman Sync
menampilkan notice "direktori state tidak bisa dipakai" beserta perintah
`install -d` untuk membuatnya manual.

```bash
sudo install -o root -g root -m 0644 garagepanel.service /etc/systemd/system/garagepanel.service
sudo systemctl daemon-reload
sudo systemctl restart garagepanel
```

Bandingkan dulu kalau ingin tahu apa yang berubah:

```bash
diff /etc/systemd/system/garagepanel.service garagepanel.service
```

### Kalau ada environment variable baru

Panel selalu berjalan dengan setelan bawaan untuk variabel yang tidak diisi,
jadi update tidak pernah memaksamu mengubah file environment. Kalau sebuah versi
menambahkan variabel baru, itu bersifat opsional dan dicatat di
[Konfigurasi](#konfigurasi). Panel menolak start dengan pesan yang jelas kalau
ada yang benar-benar wajib.

## Backup dan pemulihan

State panel kecil. Yang perlu di-backup:

| Apa | Di mana | Isinya |
|---|---|---|
| Konfigurasi panel | `/etc/garagepanel/garagepanel.env` | token admin, key S3, hash password login |
| Pemetaan domain | `/etc/caddy/sites/*.caddy` | domain → bucket, whitelist IP S3 API |
| Remote, jadwal, dan job Sync | `/var/lib/garagepanel/` | `remotes.json` (kredensial remote, 0600), `schedules.json` (jadwal), `jobs/*.json` (checkpoint), `jobs/<id>/failed.jsonl` |

```bash
sudo tar czf garagepanel-backup-$(date +%F).tar.gz \
  /etc/garagepanel/garagepanel.env \
  /etc/caddy/sites \
  /etc/sudoers.d/garagepanel \
  /etc/systemd/system/garagepanel.service \
  /var/lib/garagepanel
```

Arsip ini memuat token admin Garage, secret key S3, dan secret key setiap
remote dalam bentuk terbaca. Simpan seperti kamu menyimpan kunci SSH — jangan
ke object storage yang dikelola panel ini sendiri, dan jangan ke remote yang
kredensialnya ada di dalam arsip itu. Direktori `cache/` di dalamnya boleh
dilewati.

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
sudo rm -rf /etc/garagepanel /var/lib/garagepanel
sudo rm -f /usr/local/bin/garagepanel /etc/sudoers.d/garagepanel
sudo userdel garagepanel
sudo apt remove rclone      # kalau hanya dipasang untuk panel
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
| `S3_API_DOMAIN` | — | Domain untuk **S3 API** — yang dipakai aplikasimu. Kosong → halaman IP Whitelist nonaktif, dan kartu kredensial hanya bisa menampilkan alamat loopback |
| `LISTEN` | `127.0.0.1:8090` | **Wajib loopback.** Alamat non-loopback ditolak saat start |
| `CADDY_RELOAD_CMD` | `sudo -n /bin/systemctl reload caddy` | Perintah reload. Dipecah per spasi, **tidak** lewat shell |
| `PANEL_PASSWORD_HASH` | — | Hash password login. Kosong → tidak ada login (lihat [Login](#login-dan-akses-lewat-domain)) |
| `PANEL_USERNAME` | `admin` | Username untuk login |
| `PANEL_DOMAIN` | — | Domain untuk **UI panel** — bukan untuk S3 API. Wajib disertai `PANEL_PASSWORD_HASH` |
| `STATE_DIR` | `$STATE_DIRECTORY` dari systemd, kalau tidak ada `/var/lib/garagepanel` | Remote dan checkpoint job Sync. Tidak bisa ditulis → halaman Sync nonaktif, sisanya jalan |
| `RCLONE_BIN` | `rclone` (dicari di PATH) | Path rclone. Butuh v1.59+ |
| `RCLONE_EXTRA_ARGS` | — | Argumen tambahan untuk setiap rclone, dipecah per spasi, **tidak** lewat shell. Contoh: `--bwlimit 20M`, `--s3-sign-accept-encoding=false` |

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
| Object key | tolak yang diawali `/` atau memuat segmen `.`/`..` (dipisah `/`). `laporan..final.pdf` sah — bucket sungguhan memang punya key seperti itu — sedangkan `a/../b` ditolak |
| Nama remote | `^[a-z0-9][a-z0-9-]{0,31}$` |
| Endpoint remote | `http`/`https`, host + port saja: tanpa path, query, fragment, atau user:password; alamat link-local/metadata (`169.254.0.0/16`, `fe80::/10`) dan `0.0.0.0` ditolak |
| Region | `^[a-z0-9-]{1,32}$`; provider dari whitelist (`Wasabi`, `AWS`, `Backblaze`, `Hetzner`, `Cloudflare`, `DigitalOcean`, `Scaleway`, `Ceph`, `Minio`, `Other`); gaya alamat `auto`/`path`/`virtual` |
| Bucket remote | aturan AWS (3–63, boleh titik, bukan alamat IP) — tidak pernah jadi nama file |
| Nama folder / nama file | satu komponen: tanpa `/` `\`, bukan `.`/`..`, tanpa karakter kontrol, maks 255 byte; nama file juga lewat whitelist ekstensi |
| ID job / ID jadwal | `^[0-9a-f]{16}$`, diperiksa sebelum dijadikan path di `STATE_DIR` |
| Interval jadwal | `<angka>` + `m`/`h`/`d`, 15 menit sampai 30 hari; run pertama harus berbentuk `YYYY-MM-DDTHH:MM` |

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

**Sync.** Secret remote hanya ada di `remotes.json` (0600) dan di environment
proses rclone — tidak di argumen (`ps` tidak menampilkannya), tidak di file
job, tidak di log, tidak di halaman mana pun (access key disamarkan, secret
tidak pernah dirender). Environment rclone dibangun dari nol, jadi token admin
dan hash password panel tidak ikut. rclone tidak pernah men-list bucket dan
tidak pernah diberi kredensial selain dua remote job itu. `RCLONE_EXTRA_ARGS`
dipecah per spasi dan diberikan langsung ke `exec`, tanpa shell. Objek uji
yang ditulis Tes koneksi dan pemeriksaan sebelum job bernama
`.garagepanel-probe-<8 hex acak>` dan dihapus di request berikutnya; kalau
penghapusannya gagal, pesannya menyebut nama objek itu supaya bisa dihapus
manual. Jadwal hanya menyimpan nama remote dan setelan job, tanpa kredensial.

**Status.** Halaman Status hanya membaca file teks kernel di `/proc` dan
`/sys` serta memanggil dua operasi baca Admin API; tidak ada perintah shell,
tidak ada `systemctl`, tidak ada akses `/dev`, dan tidak ada hak tambahan di
unit systemd. Nama proses dan mount dari kernel dirender lewat
`html/template` seperti nilai lainnya.

**File manager.** Ganti nama tidak bisa memperkenalkan ekstensi di luar
whitelist (menghindari `.jpg` → `.html` yang lalu dilayani web endpoint).
Operasi massal dibatasi 1000 objek per request; operasi folder dijalankan
sebagai job dan hapus folder minta nama foldernya diketik ulang.

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
scope-nya kurang. Panel memakai sembilan operasi:
`GetClusterHealth`, `GetClusterStatus`, `ListBuckets`, `GetBucketInfo`,
`CreateBucket`, `DeleteBucket`, `UpdateBucket`, `CreateKey`,
`AllowBucketKey`. Buat ulang tokennya dengan daftar lengkap itu. Gejala yang
sama muncul kalau token ber-`--expires-in` sudah kedaluwarsa. Khusus
`GetClusterStatus` (halaman Status, daftar node Garage): tanpa scope itu
halaman Status tetap tampil dan menyebutkan scope yang kurang.

**Caddy menolak reload karena pola `import` tidak cocok** — `/etc/caddy/sites`
masih kosong. Isi placeholder:
`printf '# placeholder\n' | sudo tee /etc/caddy/sites/_placeholder.caddy`
lalu reload lagi. Nama berawalan `_` diabaikan panel.

**`Tidak bisa listen di 127.0.0.1:8090: address already in use`** — ada proses
lain di port itu, sering kali instance panel lama. Cek dengan
`sudo ss -lntp | grep 8090` (paket `iproute2`), matikan, atau ganti `LISTEN` ke
port lain.

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

**`ambiguous site definition: <domain>`** — satu alamat dideklarasikan dua kali
di config Caddy. Paling sering karena domain yang sama dipakai sebagai domain
bucket **dan** sebagai `S3_API_DOMAIN`. Panel sekarang menolak kombinasi itu
sebelum menulis, tapi kalau bentroknya ada di Caddyfile utama (di luar
jangkauan panel), cari dengan `grep -rn '<domain>' /etc/caddy/`. Pakai subdomain
terpisah: misalnya `s3.domainmu.com` untuk S3 API dan `cdn.domainmu.com` untuk
bucket.

**Kartu kredensial masih menampilkan `http://127.0.0.1:3900` sebagai Endpoint** —
itu alamat yang dipakai panel sendiri untuk bicara ke Garage, dan tidak berguna
untuk aplikasi di mesin lain. Endpoint publik diambil dari `S3_API_DOMAIN`, bukan
dari `PANEL_DOMAIN`. Isi `S3_API_DOMAIN`, restart panel, lalu tambahkan minimal
satu IP di halaman IP Whitelist supaya Caddy benar-benar mengekspos S3 API.
Setelah itu kartu kredensial menampilkan `https://<S3_API_DOMAIN>` sebagai
endpoint utama, dengan alamat loopback tetap tercantum untuk aplikasi yang
berjalan di server yang sama.

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

**Permission ditolak padahal kepemilikan sudah benar (Fedora/RHEL/Rocky —
BUKAN Ubuntu)** — SELinux kemungkinan memblokirnya. Ubuntu memakai AppArmor dan
paket Caddy resminya tidak memasang profil apa pun, jadi di Ubuntu penyebabnya
hampir pasti kepemilikan direktori, bukan LSM. Periksa dulu apakah memang itu penyebabnya:

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

**`AccessDenied: Forbidden: Invalid signature` di halaman Objek** — ini **bukan**
soal izin, dan tidak ada hubungannya dengan status public/private bucket:
halaman Objek selalu lewat S3 API (SigV4), bukan lewat web endpoint. Tanda
tangannya yang tidak cocok. Tiga penyebab, berurutan dari yang paling sering:

1. **`GARAGE_S3_SECRET_KEY` membawa spasi atau carriage return.** systemd tidak
   membuang `\r` dari EnvironmentFile berakhiran CRLF, dan hasilnya secret yang
   berbeda satu byte — Access Key ID tetap terlihat benar, jadi penyebabnya
   nyaris tak terlihat. Panel sekarang memangkasnya sendiri dan memberi
   peringatan di log, tapi periksa filenya:

   ```bash
   sudo cat -A /etc/garagepanel/garagepanel.env | grep SECRET
   ```

   Setiap baris harus berakhir `$` saja. Kalau ada `^M$`:
   `sudo sed -i 's/\r$//' /etc/garagepanel/garagepanel.env`

2. **Region tidak cocok.** `GARAGE_S3_REGION` harus sama persis dengan
   `s3_region` di `/etc/garage.toml`: `sudo grep s3_region /etc/garage.toml`.

3. **Jam server meleset** lebih dari ~15 menit — lihat
   [Langkah 0](#langkah-0--periksa-garage-dan-caddy).

Pesan errornya sendiri sudah memuat ketiga langkah ini beserta region yang
sedang dipakai panel.

**Bucket muncul di daftar tapi objeknya kosong / `AccessDenied` tanpa menyebut
signature** — kredensialnya benar, tapi key panel belum diberi izin pada bucket
itu: `garage bucket allow --read --write <bucket> --key garagepanel`.

**Halaman Sync bilang "Fitur Sync nonaktif"** — alasannya tertulis di notice.
`rclone tidak ditemukan`: `sudo apt install rclone` lalu restart panel.
`rclone vX terlalu lama`: butuh 1.59+, pakai skrip resmi rclone. `Direktori
state … tidak bisa dipakai`: unit systemd lama tanpa `StateDirectory=` —
salin ulang unit dari repo, `daemon-reload`, restart. Kalau `RCLONE_BIN`
diisi manual, pastikan file itu bisa dieksekusi oleh user `garagepanel`.

**Tes koneksi remote: `SignatureDoesNotMatch`** — secret key salah, atau region
tidak cocok dengan endpoint (Wasabi: `s3.wasabisys.com` → `us-east-1`,
`s3.<region>.wasabisys.com` → `<region>`). Klik **Edit** di kartu remote,
betulkan, lalu tes lagi. Pesan errornya menyebut region yang sedang dipakai.

**Tes koneksi remote: `InvalidAccessKeyId: Malformed Access Key Id`** —
penyedia tidak mengenali *bentuk* access key-nya; ini bukan soal izin. Di
**Backblaze B2** penyebabnya hampir selalu master application key (atau keyID
akun) yang dipakai sebagai access key — S3 API B2 hanya menerima keyID dari
*application key* yang dibuat sendiri di menu Application Keys. Buat satu
dengan akses ke bucket itu, lalu **Edit** remote dan isi keyID +
applicationKey-nya. Di **Hetzner**, pastikan kredensial S3-nya dibuat di
project yang sama dengan bucket. Di penyedia lain: periksa access key tersalin
utuh (tanpa spasi) dan endpoint memang milik akun/region itu.

**Tes koneksi remote: bisa membaca, tapi menulis objek uji ditolak
(`AccessDenied`)** — key hanya punya izin baca, bucket milik akun/project
lain, atau penyedia hanya menerima gaya alamat virtual-host sementara remote
memakai path-style (Hetzner, sebagian Ceph). Panel mencoba gaya satunya
otomatis dan menyimpan yang diterima; kalau tetap ditolak, periksa izin key di
penyedia. Job ekspor ke Hetzner yang gagal dengan `PutObject … 403
AccessDenied: UnknownError` (HostID `…-ceph…`) adalah kasus ini: bucket-nya
diakses path-style. Setelah update, jalankan Tes koneksi lagi — gaya alamatnya
diganti ke virtual-host dan disimpan, lalu klik Lanjutkan di job-nya.

**Tes koneksi remote: `PermanentRedirect` / `AuthorizationHeaderMalformed`** —
bucket ada, tapi di region atau endpoint lain dari yang diisi. Pesannya
menyebut region yang benar kalau penyedia mengirimkannya; Edit remote dan
ganti endpoint + region sepasang.

**Tes koneksi remote: `NoSuchBucket`** — nama bucket salah, atau bucket-nya ada
di akun/region lain. Nama bucket Backblaze bersifat global dan harus persis;
bucket Hetzner terikat project.

**Job gagal dengan `PutObject … 403 AccessDenied` padahal tes koneksi lulus** —
izin key atau kebijakan bucket tujuan berubah di tengah jalan. Jalankan Tes
koneksi lagi (dengan uji tulis) untuk melihat pesan aslinya, lalu Lanjutkan.

**Jadwal tidak menjalankan apa-apa** — lihat kolom "Run terakhir".
`dilewati: bucket … sedang dipakai job` berarti run sebelumnya belum selesai
(perbesar intervalnya atau biarkan); `dilewati: sumber/tujuan tidak bisa
diakses` berarti tes koneksinya gagal saat itu — jalankan Tes koneksi di
remote-nya untuk melihat pesan lengkapnya. Jadwal hanya jalan selama
`garagepanel` jalan, run yang terlewat saat panel mati tidak dirapel, dan
jadwal berstatus "dijeda" tidak pernah jalan sampai diaktifkan lagi.

**Job dijeda otomatis "… dari … objek di chunk terakhir gagal"** — salah satu
sisi tidak bisa dihubungi selama satu chunk. Periksa koneksi ke remote (atau
Garage), lalu klik Lanjutkan: chunk itu diulang, tidak ada yang dicatat gagal.

**Job gagal "rclone berhenti dengan exit code 1"** — baris log rclone terakhir
ada di bawah statusnya. Exit 1 biasanya remote tidak terdefinisi atau argumen
di `RCLONE_EXTRA_ARGS` salah; exit 5 berarti retry ke salah satu sisi habis;
exit 7 error fatal rclone. Lihat log lengkap: `journalctl -u garagepanel`, dan
kalau perlu jalankan perintah rclone yang tercantum di
[Cara kerja job](#cara-kerja-job) secara manual.

**Banyak key gagal dengan `SlowDown` / 503 dari Wasabi atau AWS** — terlalu
banyak transfer paralel untuk rate limit penyedia. Turunkan transfer paralel
di job (mulai dari 4) — rclone sudah retry 10 kali per request, jadi yang
tercatat gagal memang benar-benar ditolak.

**Job `listing sumber tidak terurut`** — penyedia mengembalikan key tidak
terurut byte-wise, yang melanggar spesifikasi S3; panel menghentikan job supaya
tidak salah banding. Laporkan ke penyedianya; untuk Garage, AWS, Wasabi, dan
MinIO ini tidak pernah terjadi.

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
main.go               konfigurasi, routing, middleware, handler bucket/domain/objek/whitelist/login
objects_ops.go        ganti nama, pindah, salin objek; operasi folder → job
garage.go             client Garage Admin API v2
s3.go                 SigV4, ListObjectsV2 (start-after), Put/Get/Head/Delete, CopyObject, DeleteObjects
caddy.go              tulis/hapus file site + reload + rollback, file whitelist, writeFileAtomic
providers.go          preset provider S3 (endpoint, region, gaya alamat bawaan, catatan)
remotes.go            daftar remote S3 (remotes.json), env rclone per remote, probe baca/tulis + deteksi gaya alamat
rclone.go             deteksi versi rclone, menjalankan satu chunk, parser log JSON
sync.go               iterator listing terurut, merge-join, penulis chunk
jobs.go               job: state di disk, siklus hidup, resume saat start, shutdown
schedules.go          jadwal berulang (schedules.json), ticker 30 detik, riwayat run
sync_http.go          halaman Sync, jobs.json, handler remote, job, dan jadwal
sysinfo.go            pembaca /proc dan /sys untuk halaman Status (CPU, memori, disk, I/O, jaringan, suhu, proses)
status_http.go        halaman Status, status.json, kesehatan dan node cluster Garage
auth.go               hash password, session, pembatas percobaan login
validate.go           semua validasi input
templates/            *.html, di-embed dengan //go:embed
garagepanel.service   unit systemd
```

Test mencakup vector SigV4 resmi AWS, round-trip parse/render file Caddy,
rollback saat reload gagal, dan integrasi seluruh handler terhadap Garage
tiruan (urutan pemanggilan API, CSRF, escaping HTML, content-addressing upload,
proteksi path traversal). Untuk Sync ada S3 tiruan sungguhan di memori
(`fake_s3_test.go`: listing terurut dengan prefix/delimiter/start-after,
multipart, CopyObject, DeleteObjects, dan pemeriksaan tanda tangan SigV4) dan
rclone tiruan (`rclone_fake_test.go`: binary test meng-exec dirinya sendiri,
memenuhi kontrak `--files-from-raw` dengan client S3 panel, mengeluarkan log
JSON yang sama, dan merekam argv + env untuk asersi). Tes resume, jeda,
pembatalan, jeda otomatis, dan batas job semuanya lewat rclone tiruan itu.

```bash
go test ./...          # cepat
go test -race ./...    # dengan race detector
go test -cover ./...   # ~80% statement coverage

# Dengan rclone sungguhan (dilewati kalau tidak ada): 302 objek termasuk satu
# 20 MiB multipart, antar dua S3 tiruan
GARAGEPANEL_TEST_RCLONE=/usr/bin/rclone go test -run TestRealRclone ./...
```

Menjalankan panel lokal tanpa server Garage sungguhan: lihat
`newTestPanel` di `main_test.go` — isinya Garage + S3 + web endpoint + rclone
tiruan yang bisa dipakai ulang.
