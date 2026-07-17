# rc-loc

Easy localization tool for Windows Resource (`.rc`) files, specifically optimized for **Wolf RPG Editor**.

This tool allows you to extract strings from `.rc` files, translate them (manually or automatically via Google Translate), and apply them back while preserving the correct encoding and Windows-specific formatting.

## Features

- **Extraction**: Extract all strings from `.rc` files into a manageable JSON format.
- **Merge**: Update existing translations when the source `.rc` file changes (preserves your work, marks removed strings).
- **Auto-Translate**: One-click translation using Google Translate (no API key required).
- **Professional Workflow**: Export to `.po` (GNU gettext) format for use with **Poedit**, **Weblate**, or **Crowdin**.
- **Windows Compatibility**: Full support for UTF-16LE encoding and Windows accelerator markers (`&`).

## Getting Started

### Installation
Since this is written in Go, you can build it from source:
```bash
go build -o rc-loc main.go
```

### Usage

The tool supports several commands:

| Command | Description |
| :--- | :--- |
| `extract` | Extract strings from `.rc` $\to$ `.json` |
| `merge` | Update `.json` with new strings from updated `.rc` |
| `translate`| Auto-translate `.json` using Google Translate |
| `apply` | Apply translations from `.json` $\to$ `.rc` (UTF-16LE) |
| `export-po` | Export `.json` $\to$ `.po` (for Poedit/Weblate) |
| `import-po` | Import `.po` $\to$ `.json` |

### Example Workflow

**1. First time translation:**
```bash
./rc-loc extract  MDS.rc strings.json
./rc-loc translate strings.json strings_ru.json --from en --to ru
./rc-loc apply     MDS.rc strings_ru.json MDS_RU.rc
```

**2. When the game updates (RC file changes):**
```bash
./rc-loc merge     MDS_new.rc strings_ru.json
./rc-loc apply     MDS_new.rc strings_ru.json MDS_RU.rc
```

**3. Community translation via Poedit:**
```bash
./rc-loc export-po strings.json strings.po
# ... edit strings.po in Poedit ...
./rc-loc import-po strings.po strings.json
./rc-loc apply     MDS.rc strings.json MDS_RU.rc
```

## 🛠 Technical Details
- **Encoding**: Handles UTF-16LE (BOM) automatically.
- **Placeholders**: Protects C-style escapes and printf-style placeholders (`%s`, `%d`, etc.) during translation.
- **Accelerators**: Automatically handles and restores Windows menu mnemonics (`&`).

## [License](LICENSE)
