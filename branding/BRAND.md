# Naparnik Admin — Brand Kit v1.0

## Концепция
N — арка и точка входа, A — золотая дорога, управляемый маршрут. Единый знак NA.

## Цвета
- Graphite: `#141B25`
- Gold: `#FFC400`
- White: `#FFFFFF`
- Dark gold for text on light backgrounds: `#C58A00` (для крупного логотипа, не мелкого текста).

## Файлы
- `github/github-banner.png`: исходный широкий баннер, 1672×407.
- `github/social-preview.png`: 1280×640, загрузить в GitHub Settings → Social preview.
- `ui/logo-light.svg`, `ui/logo-dark.svg`: горизонтальные логотипы для соответствующих тем.
- `ui/logo-stacked-*.svg`: двухстрочные версии.
- `ui/symbol-*.svg`: знак без текста; `symbol.svg` — светлая версия.
- `icons/app.ico`: Windows, 16/24/32/48/64/128/256 px.
- `icons/favicon.ico`: 16/32/48 px; `favicon.svg` — современный браузер.
- `icons/app.png`: 512×512, прозрачные углы.
- `reference/approved-concept.png`: утверждённый визуальный референс.

## Интеграция
README: `![Naparnik Admin](assets/brand/github/github-banner.png)`
HTML: `<link rel="icon" type="image/svg+xml" href="/favicon.svg">` и `<link rel="alternate icon" href="/favicon.ico">`.
Пути примерные; адаптировать к фактической структуре проекта. ICO подключить через используемый механизм Windows resource embedding. Проверить итоговый EXE, favicon, обе темы UI. Не менять логику приложения.

SVG содержат контуры текста, не требуют установки шрифтов. Не растягивать знак, не добавлять тени в UI. В малых размерах использовать только иконку.

## Ограничения
Плоский знак вручную реконструирован по утверждённому растровому макету, а не извлечён из исходного вектора. Баннер взят из утверждённого изображения; Social Preview адаптирован на тёмном фоне. Интеграция в репозиторий и проверка сборки не выполнялись.
