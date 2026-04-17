# rmouse — использование

Документация по установке, запуску и настройке под каждую платформу.
Краткий обзор архитектуры — в корневом [README](../README.md).

Бинарники двух категорий:

- **CLI** — `mouse-remote-server`, `mouse-remote-client`, `mouse-remote-relay`.
  Headless, кроссплатформенные, управляются флагами.
- **GUI** (Wails) — `mouse-remote-server-gui`, `mouse-remote-client-gui`.
  Десктопные окна с настройками, визуальной раскладкой мониторов и
  drag-and-drop расстановкой клиентов.

Типовой сценарий: **GUI-сервер** на машине с физической мышью + **GUI- или CLI-клиент** на каждой удалённой. Relay — опциональный компонент для NAT-ситуаций.

---

## Установка

### Вариант 1: готовые бинарники

Скачать с [Releases](https://github.com/titrom/rmouse/releases/latest) архив под свою платформу и распаковать куда-нибудь в PATH или в любую удобную папку.

- `rmouse-windows-amd64.zip` — CLI + GUI под Windows.
- `rmouse-linux-amd64.tar.gz` — CLI под Linux (и GUI, если CI-workflow под Linux GUI включён; см. ниже).

### Вариант 2: собрать из исходников

Требования:

- **Go** ≥ 1.25.3.
- Для GUI: **Node.js** ≥ 20 и [**Wails CLI**](https://wails.io/docs/gettingstarted/installation):
  ```
  go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0
  ```
- На **Linux** для GUI нужны системные пакеты GTK3 + WebKit2GTK:
  ```
  sudo apt-get install libgtk-3-dev libwebkit2gtk-4.1-dev
  ```

Сборка всех бинарников сразу — скрипт `scripts/build-all.sh` (из корня
репо):

```bash
./scripts/build-all.sh
```

Он складывает бинарники в `build/<os>-<arch>/`. CLI кроссуется отовсюду, Wails GUI — только под ту ОС, на которой запускаете сборку (ограничение Wails).

Ручная сборка отдельного бинаря:

```bash
# CLI
go build -o mouse-remote-server ./cmd/mouse-remote-server

# GUI (из каталога cmd/...)
cd cmd/mouse-remote-server-gui
wails build -clean
# результат: cmd/mouse-remote-server-gui/build/bin/mouse-remote-server-gui[.exe]
```

---

## Первый запуск

### Общее

1. Задайте общий **pairing token** — любая строка длиной от 12 символов,
   одинаковая на сервере и всех клиентах. GUI хранит token в OS keyring
   (Windows Credential Manager / GNOME Keyring / KWallet), CLI принимает
   через флаг `--token`.
2. Сервер при первом запуске генерирует self-signed сертификат в
   `%AppData%\rmouse\` / `~/.config/rmouse/`. SHA-256 fingerprint показан в
   GUI — клиент сверяет его при первом подключении.

### Требования платформы

| Платформа | Захват мыши (сервер)          | Инжект мыши (клиент)       |
| --------- | ----------------------------- | -------------------------- |
| Windows   | WinAPI hooks — без привилегий | `SendInput` — без привилегий |
| Linux/X11 | `/dev/uinput` — нужен root или `input` group | XTest — без привилегий (нужен запущенный X-сервер и `$DISPLAY`) |

Linux **Wayland** на данный момент не поддерживается — захват и инжект
завязаны на X11 протокол. На Wayland-сессиях работает только клиент в
режиме чтения (видит окно GUI, но захват входа с серверной стороны
невозможен).

Linux-серверу нужен доступ к `/dev/uinput`:

```bash
sudo usermod -a -G input "$USER"
sudo chmod 0660 /dev/uinput
# перелогиниться
```

---

## Запуск: GUI

### Сервер

1. Запустить `mouse-remote-server-gui`.
2. Поле **Token** — тот самый pairing secret.
3. **Bind address** (default `0.0.0.0:24242`) — порт, на котором слушать.
   Оставьте `0.0.0.0`, если клиенты в той же локалке; замените на адрес
   конкретного интерфейса при multi-homing.
4. Кнопка **Start**. Статус в правом углу → `running`.
5. В секции **Screen** появится раскладка физических мониторов хоста. Когда
   подключится клиент, его экран появится справа от серверного, flush к
   правой кромке.
6. **Drag-and-drop**: схватите клиентский экран мышью и перетащите в
   нужную позицию. Во время drag'а:
   - Вокруг каждого существующего экрана появляются полосы клеток
     (halo) в направлениях, куда можно примкнуть новый клиент.
   - Ярким синим outline'ом показан **ghost rect** — куда клиент сядет
     при отпускании; соответствующие клетки grid'а тоже подсвечиваются.
   - Сетка выровнена по конфигурируемому шагу **`grid px`** (поле в
     header'е Screen-панели, default 240 world-пикселей).
7. Отпустите — клиент приземляется в ближайшую свободную ячейку
   (round-to-grid с post-snap collision check; если rounding упирается
   в server-bbox, ring-search ищет ближайшую свободную по grid'у).

Расстановка (`placements`) записывается в
`%AppData%\rmouse\server-gui.json` / `~/.config/rmouse/server-gui.json`
и восстанавливается при следующем запуске. При переподключении клиент
возвращается ровно туда, где его оставили.

**Hotplug мониторов** хоста (подключили/отключили второй монитор,
сменили разрешение) обрабатывается автоматически — сервер пересчитывает
bbox, GUI перерисовывает раскладку.

### Клиент

1. Запустить `mouse-remote-client-gui`.
2. **Server address** — адрес сервера (`192.168.1.10:24242` для LAN
   или `relay.example.com:24243` + session для relay-режима).
3. **Token** — тот же, что на сервере.
4. **Cert fingerprint** — скопировать с GUI-сервера и вставить сюда при
   первом подключении (trust-on-first-use).
5. **Connect**.

После подключения курсор сервера, пересекая край серверного монитора в
сторону клиента, телепортируется на клиентский экран; обратное движение
возвращает управление серверу. Между сервером и клиентом может быть
визуальный gap в GUI — курсор перепрыгивает его (на сервере: raycast в
сторону push'а; на клиенте: gap-teleport на server-facing кромке при
release).

---

## Запуск: CLI

Полезно, если GUI под Linux ещё не собран или нужен headless-сервер.

### LAN (прямой)

```bash
# сервер
./mouse-remote-server --listen 0.0.0.0:24242 --token "$TOKEN"

# клиент
./mouse-remote-client --connect 192.168.1.10:24242 --token "$TOKEN"
```

CLI-сервер не поддерживает drag-and-drop расстановку; клиент встаёт
flush справа от серверного bbox и остаётся там до перезапуска с другими
параметрами.

### Relay (NAT / CGNAT)

Если прямое соединение невозможно (разные VPN на концах, симметричный
NAT и т.п.), запускайте `mouse-remote-relay` на любой машине с публичным
IP. TLS и token остаются end-to-end — relay видит только зашифрованный
трафик.

```bash
# на VPS (любой Linux с публичным IP)
./mouse-remote-relay --listen 0.0.0.0:24243

# общий session-id
openssl rand -hex 16   # => <SESSION>

# сервер
./mouse-remote-server --relay vps.example.com:24243 --session <SESSION> --token "$TOKEN"
# клиент
./mouse-remote-client --relay vps.example.com:24243 --session <SESSION> --token "$TOKEN"
```

При заданном `--relay` флаги `--listen` / `--connect` игнорируются.

---

## Настройка сетки размещения

Шаг сетки (`gridStep`) задаётся в UI сервера и хранится в `localStorage`
(`rmouse.gridStep`). Это чисто визуальный / snap-параметр — роутер
о нём не знает, он получает абсолютные (x, y) координаты placement'а.

- **Меньше** шаг → точнее позиционирование, больше подсветки halo-клеток.
- **Больше** шаг → меньше "дребезга" при drop'е, grid выглядит крупнее.

Default 240 — подобрано под типичные 1920×1080 / 1920×1200 мониторы
(≈12 клеток поперёк монитора). Для 4K комфортнее 480–500.

Коллизии:

- Во время drag курсор не может затащить клиента **внутрь** любого
  другого монитора (server / other client) — движение по axis
  блокируется на кромке.
- После drop'а если rounding всё же попадает внутрь, `findFreeGridSpot`
  делает ring-search на grid'е (до 30 колец) и приземляет в ближайшую
  свободную ячейку. Если совсем ничего не находит — откат к позиции
  начала drag'а.

Диагональное размещение поддерживается — и сам snap, и grab (через
raycast для любой кардинальной push-оси).

---

## Troubleshooting

**Клиент не подключается**: проверьте fingerprint на сервере
(`CertFingerprint` в GUI или `~/.config/rmouse/server.crt` →
`openssl x509 -noout -fingerprint -sha256 -in …`). Token чувствителен к
пробелам и регистру.

**Курсор "туда смог, обратно нет"** (старый баг, починен): если
наблюдаете на старой сборке — обновитесь. Gap-cross в обе стороны:
raycast на push, teleport-across-gap на release.

**Залипают модификаторы** (Ctrl/Shift после grab-in): починено в
router; если воспроизводится, соберите лог из
`%TEMP%\rmouse-server.log` (на Windows GUI) — в нём есть детальный trace
grab-переходов.

**Hotplug не отрабатывает на Linux**: Subscribe использует X RandR; на
Wayland это не работает. Рестарт сервера перечитает раскладку.

**"Token required" на старте**: заполните поле в GUI или передайте
`--token`. Пустой token запрещён явно — не даём случайно стартовать
open-сервер.

---

## Логи

- **GUI сервер (Windows)**: `%TEMP%\rmouse-server.log` — slog в текстовом
  виде. GUI на Windows не имеет stderr-console, поэтому всё туда.
- **CLI**: stderr, уровни info/warn/error. Запускать с `2> log.txt` для
  записи.
- **Конфиги**: `%AppData%\rmouse\` (Windows), `~/.config/rmouse/`
  (Linux).
