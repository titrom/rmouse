# rmouse

Go-утилита для объединения нескольких компьютеров в одно рабочее пространство: одна физическая мышь и клавиатура управляют всеми машинами, курсор перетекает с экрана на экран при пересечении границы. Аналог Synergy / Barrier / Mouse Without Borders.

Полное руководство по установке, запуску и UX — в [docs/USAGE.md](docs/USAGE.md).
Готовые бинарники — на странице [Releases](https://github.com/titrom/rmouse/releases/latest).

## Поддерживаемые ОС

- Windows (WinAPI hooks + SendInput)
- Linux / X11 (evdev + uinput захват, XTest инжект)

Wayland пока не поддерживается.

## Архитектура

Один сервер (хост с физической мышью/клавиатурой) + N клиентов. TLS 1.3, pre-shared pairing token. У сервера есть GUI для визуальной расстановки клиентов вокруг физических мониторов: drag-and-drop на сетку с настраиваемым шагом, snap-to-grid с ring-search при коллизиях, halo-подсказки валидных позиций во время перетаскивания. Hotplug мониторов хоста (подключение/отключение/смена разрешения) отрабатывается онлайн без перезапуска сервера.

Подробности по UX, CLI-флагам, relay и troubleshooting'у — в [docs/USAGE.md](docs/USAGE.md).

## Сборка

Скрипт, собирающий все бинарники под текущий хост:

```bash
./scripts/build-all.sh      # → build/<os>-<arch>/
```

Или вручную (CLI кроссуется с любого хоста; Wails GUI — только под ту ОС, на которой собираете):

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
