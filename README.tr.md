<p align="center">
  <img src="docs/logo.svg" width="96" height="96" alt="Runner Deck logosu">
</p>

<h1 align="center">Runner Deck</h1>

<p align="center">
  Self-hosted GitHub Actions runner'larını tek ekrandan ekle, durdur ve kaynak kullanımını izle.
</p>

<p align="center">
  <a href="README.md">English</a> · <b>Türkçe</b>
</p>

![Runner Deck sistem kaynakları ekranı](docs/images/system-tr.png)

## Neden?

Her repo için ayrı compose dosyası tutmak yerine runner'larını tek arayüzden yönetirsin. Bir repoya 1 runner, diğerine 3 runner vermek birkaç tıklama sürer. Hangisinin ne kadar CPU, bellek ve disk tükettiğini de aynı yerden görürsün.

- **Repo bazında runner:** GitHub'ın verdiği `./config.sh` satırını yapıştır, form kendini doldursun.
- **Sistem kaynakları:** runner başına CPU, bellek ve disk, oturum grafikleri ve Docker disk kullanımı.
- **Her runner kendi konteynerinde:** work dizinleri ayrı, checkout'lar birbirini ezmez.
- **Veritabanı yok:** tüm yapılandırma tek YAML dosyasında.
- **Yedi dil:** Türkçe, English, 中文, Français, Deutsch, 한국어, 日本語.
- **Açık ve koyu tema**, telefonda da çalışır.

![Runner listesi](docs/images/runners-tr.png)

## Coolify'a kurulum

1. Coolify'da bu repodan **Docker Compose** kaynağı oluştur. Compose yolu varsayılan `docker-compose.yml` kalsın.
2. Domain otomatik gelmez: `runner-manager` servisine domaini `https://alanadin.com:8080` biçiminde yazıp deploy et.
3. Giriş parolası Coolify'ın ürettiği `SERVICE_PASSWORD_RUNNERDECK` değişkenindedir, kullanıcı adı `admin`.
4. Arayüzden **Hızlı runner ekle** ile ilk runner'ını ekle.

Başka bir ortama kurulum için [Kullanım Kılavuzu](docs/guide.md) ve [örnekler](examples/deploy/) klasörüne bak.

## Güvenlik

- **Yalnızca private repo bağla.** Public repoda pull request açabilen herkes bu makinede kod çalıştırabilir.
- **Docker soketi host'ta root demektir.** Yönetici arayüzüne giren runner ekleyip kod çalıştırabilir, bu yüzden parolayı güçlü tut.
- Ayrıntılar: [SECURITY.md](SECURITY.md).

Ekran görüntülerindeki veriler örnektir.

## Teşekkürler

Bu projenin başlangıç noktası olan [soulteary/runner-fleet](https://github.com/soulteary/runner-fleet) projesine teşekkürler.
