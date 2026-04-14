# rmouse

Go-утилита для объединения нескольких компьютеров в одно рабочее пространство: одна физическая мышь и клавиатура управляют всеми машинами, курсор перетекает с экрана на экран при пересечении границы. Аналог Synergy / Barrier / Mouse Without Borders.

## Статус

В разработке. Milestone M1 (Echo over TLS).

## Поддерживаемые ОС

- Windows (WinAPI hooks + SendInput)
- Linux / X11 (evdev + uinput)

## Архитектура

Один сервер (хост с физической мышью/клавиатурой) + N клиентов. TLS 1.3, pre-shared pairing token.

## Сборка

```bash
go build ./cmd/mouse-remote-server
go build ./cmd/mouse-remote-client
go build ./cmd/mouse-remote-relay
```

## Режимы соединения

### Прямой (LAN)

Сервер слушает, клиент подключается:

```bash
./mouse-remote-server --listen 0.0.0.0:24242 --token SECRET
./mouse-remote-client --connect 192.168.1.10:24242 --token SECRET
```

### Через relay

Используется, когда прямое соединение невозможно (разные VPN на обеих машинах, CGNAT, симметричный NAT). Оба пира делают исходящее соединение к публичному relay; тот склеивает TCP-стримы. TLS и pairing token остаются end-to-end — relay видит только шифротекст.

**На VPS с публичным IP (любой Linux, VPN не нужен):**

```bash
./mouse-remote-relay --listen 0.0.0.0:24243
```

**Сгенерировать общий session id** (это секрет — знание его даёт право занять сторону в rendezvous):

```bash
openssl rand -hex 16
```

**Сервер (домашняя машина) и клиент (вторая домашняя машина)** используют одинаковые `--relay` и `--session`:

```bash
./mouse-remote-server --relay vps.example.com:24243 --session <sess> --token SECRET
./mouse-remote-client --relay vps.example.com:24243 --session <sess> --token SECRET
```

Флаги `--listen` / `--connect` при заданном `--relay` не используются.
