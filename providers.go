package main

import (
	"fmt"
	"strings"
)

// Each S3 provider has its own endpoint pattern, region convention and
// addressing style, and its own way of saying "wrong key". This table is what
// the remote form, the probes and the error hints draw on.

// Addressing styles for a remote.
const (
	AddressingAuto    = "auto"    // provider default first, the other style on failure
	AddressingPath    = "path"    // https://host/bucket/key
	AddressingVirtual = "virtual" // https://bucket.host/key
)

// providerInfo describes one provider preset.
type providerInfo struct {
	// Name is the value stored in remotes.json and shown in the UI.
	Name string
	// Label is the human name.
	Label string
	// Rclone is the value handed to rclone as "provider". rclone that does
	// not know a name logs a notice and uses generic defaults, which is what
	// "Other" gives anyway.
	Rclone string
	// Virtual is the default addressing style.
	Virtual bool
	// Endpoint and Region are placeholders/examples for the form.
	Endpoint string
	Region   string
	// Note is shown under the form when the provider is selected.
	Note string
}

var providers = []providerInfo{
	{
		Name: "Wasabi", Label: "Wasabi", Rclone: "Wasabi",
		Endpoint: "https://s3.ap-southeast-1.wasabisys.com", Region: "ap-southeast-1",
		Note: "us-east-1 memakai https://s3.wasabisys.com; region lain https://s3.<region>.wasabisys.com. Region harus cocok dengan endpoint.",
	},
	{
		Name: "AWS", Label: "Amazon S3", Rclone: "AWS", Virtual: true,
		Endpoint: "https://s3.ap-southeast-1.amazonaws.com", Region: "ap-southeast-1",
		Note: "Endpoint dan region harus region tempat bucket berada. Pakai IAM user dengan izin s3:ListBucket, s3:GetObject, s3:PutObject, s3:DeleteObject.",
	},
	{
		Name: "Backblaze", Label: "Backblaze B2", Rclone: "Other",
		Endpoint: "https://s3.us-west-004.backblazeb2.com", Region: "us-west-004",
		Note: "Endpoint ada di halaman bucket B2 (s3.<region>.backblazeb2.com); region = bagian <region> itu. Access key = keyID dari application key yang dibuat manual — master application key TIDAK bisa dipakai di S3 API.",
	},
	{
		Name: "Hetzner", Label: "Hetzner Object Storage", Rclone: "Hetzner", Virtual: true,
		Endpoint: "https://fsn1.your-objectstorage.com", Region: "fsn1",
		Note: "Region = lokasi (fsn1, nbg1, hel1). Hetzner hanya melayani gaya alamat virtual-host (bucket.fsn1.your-objectstorage.com); kredensial S3 berlaku per project, jadi bucket dan key harus dari project yang sama.",
	},
	{
		Name: "Cloudflare", Label: "Cloudflare R2", Rclone: "Cloudflare",
		Endpoint: "https://<account-id>.r2.cloudflarestorage.com", Region: "auto",
		Note: "Endpoint memakai account ID; region selalu \"auto\". Token API R2 dengan izin Object Read & Write.",
	},
	{
		Name: "DigitalOcean", Label: "DigitalOcean Spaces", Rclone: "DigitalOcean",
		Endpoint: "https://sgp1.digitaloceanspaces.com", Region: "sgp1",
		Note: "Region = kode datacenter Spaces (sgp1, nyc3, ams3, …) yang sama dengan endpoint.",
	},
	{
		Name: "Scaleway", Label: "Scaleway Object Storage", Rclone: "Scaleway",
		Endpoint: "https://s3.fr-par.scw.cloud", Region: "fr-par",
		Note: "Region fr-par, nl-ams, atau pl-waw, sama dengan endpoint.",
	},
	{
		Name: "Ceph", Label: "Ceph RGW", Rclone: "Ceph",
		Endpoint: "https://s3.example.com", Region: "default",
		Note: "Region biasanya nama zonegroup (sering \"default\"). Kalau server hanya menerima virtual-host, pilih gaya alamat itu atau biarkan otomatis.",
	},
	{
		Name: "Minio", Label: "MinIO", Rclone: "Minio",
		Endpoint: "http://minio.lan:9000", Region: "us-east-1",
		Note: "Region bawaan MinIO adalah us-east-1 kecuali diubah di servernya.",
	},
	{
		Name: "Other", Label: "Lainnya (Garage lain, dll.)", Rclone: "Other",
		Endpoint: "http://10.0.0.5:3900", Region: "garage",
		Note: "Garage lain: region = s3_region di garage.toml-nya. Penyedia lain: ikuti dokumentasinya.",
	},
}

// providerByName looks a preset up; ok is false for unknown names.
func providerByName(name string) (providerInfo, bool) {
	for _, p := range providers {
		if p.Name == name {
			return p, true
		}
	}
	return providerInfo{}, false
}

// providerNames lists the accepted provider values.
func providerNames() []string {
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		out = append(out, p.Name)
	}
	return out
}

// ValidateProvider checks the provider name against the presets.
func ValidateProvider(s string) error {
	if _, ok := providerByName(s); ok {
		return nil
	}
	return fmt.Errorf("provider %q tidak dikenal; pilih salah satu dari %s", s, strings.Join(providerNames(), ", "))
}

// ValidateAddressing checks an addressing style; empty means auto.
func ValidateAddressing(s string) error {
	switch s {
	case "", AddressingAuto, AddressingPath, AddressingVirtual:
		return nil
	}
	return fmt.Errorf("gaya alamat %q tidak dikenal (auto, path, virtual)", s)
}

// addressingLabel is the style in words.
func addressingLabel(virtual bool) string {
	if virtual {
		return "virtual-host (bucket.host)"
	}
	return "path-style (host/bucket)"
}
