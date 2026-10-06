# Benchmark de genix-s3

IDs: row numbers. Corpus: 10010 productos, 225 consultas. Se genera con `GXS_BENCH=1 go test ./internal/query -run Bench -v`.

Efectividad contra los juicios de relevancia (qrels): S@1, S@10 y MRR sobre las consultas con objetivo; nDCG@10 y P@10 sobre todas salvo las negativas; falsos es la fracción del top 10 con nota 0 (en las negativas, si devolvió algo). R1 es BM25 (FTS5), R2 BM25 de trigramas, R3 nuestras reglas sin índice. El solapamiento con R1 es parecido, no calidad.

## Base A: nombres

Índice (es100): .idx 164 KB · .db 0 KB · tokens 134.4 KB · p.log 0 KB · 99 objetos · build 61 ms, 102 PUTs, 676 KB escritos, $0.000117.

### Efectividad

| Categoría | N | S@1 · S@10 · MRR | nDCG@10 · P@10 · falsos | R1 nDCG | R2 nDCG | R3 nDCG | Solapamiento R1 |
|---|---|---|---|---|---|---|---|
| exacta | 35 | 0.89 · 1.00 · 0.94 | 0.97 · 0.17 · 0.04 | 0.97 | 0.88 | 0.97 | 0.65 |
| parcial | 20 | 0.85 · 1.00 · 0.93 | 0.98 · 0.16 · 0.00 | 0.97 | 0.89 | 0.98 | 0.61 |
| escribiendo | 25 | 0.84 · 1.00 · 0.88 | 0.97 · 0.24 · 0.02 | 0.95 | 0.86 | 0.97 | 0.61 |
| typo | 25 | 0.72 · 0.92 · 0.79 | 0.89 · 0.15 · 0.03 | 0.75 | 0.84 | 0.89 | 0.45 |
| tildes | 15 | 0.87 · 1.00 · 0.93 | 0.98 · 0.22 · 0.01 | 0.94 | 0.90 | 0.98 | 0.48 |
| marca | 15 | - | 1.00 · 0.63 · 0.00 | 0.93 | 0.98 | 1.00 | 0.66 |
| numeros | 15 | 0.73 · 1.00 · 0.83 | 0.96 · 0.24 · 0.00 | 0.88 | 0.75 | 0.96 | 0.59 |
| plural | 25 | 0.80 · 1.00 · 0.90 | 0.98 · 0.17 · 0.00 | 0.81 | 0.90 | 0.98 | 0.56 |
| nombre+grupo (sin grupo) | 25 | 0.28 · 0.60 · 0.36 | 0.66 · 0.11 · 0.03 | 0.54 | 0.60 | 0.66 | 0.37 |
| negativa | 10 | - | - · - · 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| todas | 210 | 0.75 · 0.94 · 0.82 | 0.93 · 0.21 · 0.02 | 0.85 | 0.84 | 0.93 | 0.53 |

### Contra las referencias (todas las consultas de la base)

| Sistema | S@1 | S@10 | MRR | nDCG@10 | P@10 | falsos |
|---|---|---|---|---|---|---|
| genix-s3 | 0.75 | 0.94 | 0.82 | 0.93 | 0.21 | 0.02 |
| R1 BM25 | 0.61 | 0.86 | 0.71 | 0.85 | 0.19 | 0.02 |
| R2 BM25 trigramas | 0.62 | 0.91 | 0.72 | 0.84 | 0.20 | 0.18 |
| R3 nuestras reglas sin índice | 0.75 | 0.94 | 0.82 | 0.93 | 0.21 | 0.02 |
| R3 con palabras completas (sin el corte de 8 letras) | 0.71 | 0.92 | 0.79 | 0.91 | 0.21 | 0.01 |

### Costo (solo genix-s3, en frío)

S3 Express One Zone us-east-1: GET $0.00003 por 1000, lectura $0.0006 por GB.

| Categoría | $ por 1M búsquedas prom. · p95 | GET prom. · p50 · p95 · máx | KB prom. · p50 · p95 · máx | Rondas |
|---|---|---|---|---|
| exacta | 0.1146 · 0.1264 | 3.69 · 4 · 4 · 4 | 7.0 · 6.7 · 11.2 · 13.3 | 1.00 |
| parcial | 0.1170 · 0.1269 | 3.75 · 4 · 4 · 4 | 7.8 · 7.6 · 12.0 · 12.8 | 1.00 |
| escribiendo | 0.1107 · 0.1267 | 3.56 · 4 · 4 · 4 | 6.9 · 6.2 · 11.7 · 12.3 | 1.00 |
| typo | 0.1146 · 0.1273 | 3.68 · 4 · 4 · 4 | 7.4 · 7.5 · 12.7 · 13.8 | 1.00 |
| tildes | 0.1205 · 0.1273 | 3.87 · 4 · 4 · 4 | 7.8 · 7.6 · 12.8 · 12.8 | 1.00 |
| marca | 0.1080 · 0.1565 | 3.47 · 3 · 5 · 5 | 7.0 · 7.8 · 11.3 · 11.3 | 1.00 |
| numeros | 0.1465 · 0.2454 | 4.73 · 5 · 8 · 8 | 7.9 · 8.5 · 10.1 · 10.1 | 1.00 |
| plural | 0.1109 · 0.1268 | 3.56 · 4 · 4 · 4 | 7.2 · 6.7 · 11.8 · 13.9 | 1.00 |
| nombre+grupo | 0.1196 · 0.1570 | 3.84 · 4 · 5 · 6 | 7.7 · 7.2 · 12.3 · 14.3 | 1.00 |
| negativa | 0.0675 · 0.0924 | 2.20 · 2 · 3 · 3 | 2.6 · 2.7 · 4.5 · 4.5 | 1.00 |
| todas | 0.1145 · 0.1548 | 3.68 · 4 · 5 · 8 | 7.1 · 7.0 · 12.0 · 14.3 | 1.00 |

En frío, promedio por consulta: GET .idx 2.68 · .db 0.00 · p.log 1.00 · otros 0.00; KB .idx 7.0 · .db 0.0 · p.log 0.2 · otros 0.0.

## Base B: nombres + grupos (categoría)

Índice (es100): .idx 192 KB · .db 0 KB · tokens 145.1 KB · p.log 0 KB · 99 objetos · build 75 ms, 102 PUTs, 852 KB escritos, $0.000118.

### Efectividad

| Categoría | N | S@1 · S@10 · MRR | nDCG@10 · P@10 · falsos | R1 nDCG | R2 nDCG | R3 nDCG | Solapamiento R1 |
|---|---|---|---|---|---|---|---|
| exacta | 35 | 0.89 · 1.00 · 0.94 | 0.97 · 0.17 · 0.04 | 0.97 | 0.90 | 0.97 | 0.65 |
| parcial | 20 | 0.85 · 1.00 · 0.93 | 0.98 · 0.16 · 0.00 | 0.98 | 0.92 | 0.98 | 0.61 |
| escribiendo | 25 | 0.84 · 1.00 · 0.88 | 0.97 · 0.24 · 0.02 | 0.95 | 0.85 | 0.97 | 0.64 |
| typo | 25 | 0.72 · 0.92 · 0.79 | 0.89 · 0.15 · 0.03 | 0.75 | 0.86 | 0.89 | 0.46 |
| tildes | 15 | 0.87 · 1.00 · 0.93 | 0.98 · 0.22 · 0.01 | 0.95 | 0.92 | 0.98 | 0.48 |
| marca | 15 | - | 1.00 · 0.63 · 0.00 | 0.93 | 0.98 | 1.00 | 0.65 |
| numeros | 15 | 0.73 · 1.00 · 0.83 | 0.96 · 0.24 · 0.00 | 0.88 | 0.78 | 0.96 | 0.62 |
| plural | 25 | 0.80 · 1.00 · 0.90 | 0.98 · 0.17 · 0.00 | 0.81 | 0.93 | 0.98 | 0.56 |
| nombre+grupo | 25 | 0.52 · 0.96 · 0.68 | 0.89 · 0.20 · 0.02 | 0.91 | 0.86 | 0.89 | 0.50 |
| grupo | 15 | - | 0.59 · 0.30 · 0.15 | 0.80 | 0.70 | 0.59 | 0.33 |
| negativa | 10 | - | - · - · 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| todas | 225 | 0.78 · 0.98 · 0.86 | 0.93 · 0.23 · 0.03 | 0.90 | 0.87 | 0.93 | 0.53 |

### Contra las referencias (todas las consultas de la base)

| Sistema | S@1 | S@10 | MRR | nDCG@10 | P@10 | falsos |
|---|---|---|---|---|---|---|
| genix-s3 | 0.78 | 0.98 | 0.86 | 0.93 | 0.23 | 0.03 |
| R1 BM25 | 0.69 | 0.95 | 0.79 | 0.90 | 0.22 | 0.02 |
| R2 BM25 trigramas | 0.71 | 0.97 | 0.81 | 0.87 | 0.22 | 0.16 |
| R3 nuestras reglas sin índice | 0.78 | 0.98 | 0.86 | 0.93 | 0.23 | 0.03 |

### Costo (solo genix-s3, en frío)

S3 Express One Zone us-east-1: GET $0.00003 por 1000, lectura $0.0006 por GB.

| Categoría | $ por 1M búsquedas prom. · p95 | GET prom. · p50 · p95 · máx | KB prom. · p50 · p95 · máx | Rondas |
|---|---|---|---|---|
| exacta | 0.1153 · 0.1280 | 3.69 · 4 · 4 · 4 | 8.2 · 8.0 · 13.9 · 17.0 | 1.00 |
| parcial | 0.1176 · 0.1278 | 3.75 · 4 · 4 · 4 | 9.0 · 8.6 · 13.7 · 14.4 | 1.00 |
| escribiendo | 0.1117 · 0.1274 | 3.56 · 4 · 4 · 4 | 8.5 · 8.1 · 13.0 · 15.6 | 1.00 |
| typo | 0.1155 · 0.1281 | 3.68 · 4 · 4 · 4 | 8.8 · 9.2 · 14.2 · 16.8 | 1.00 |
| tildes | 0.1214 · 0.1294 | 3.87 · 4 · 4 · 4 | 9.5 · 9.1 · 16.3 · 16.3 | 1.00 |
| marca | 0.1086 · 0.1582 | 3.47 · 3 · 5 · 5 | 8.1 · 9.4 · 14.4 · 14.4 | 1.00 |
| numeros | 0.1472 · 0.2459 | 4.73 · 5 · 8 · 8 | 9.1 · 9.3 · 12.7 · 12.7 | 1.00 |
| plural | 0.1119 · 0.1288 | 3.56 · 4 · 4 · 4 | 8.8 · 8.5 · 15.3 · 15.8 | 1.00 |
| nombre+grupo | 0.1209 · 0.1587 | 3.84 · 4 · 5 · 6 | 9.9 · 9.8 · 15.3 · 17.4 | 1.00 |
| grupo | 0.1123 · 0.1274 | 3.60 · 4 · 4 · 4 | 7.5 · 7.5 · 12.9 · 12.9 | 1.00 |
| negativa | 0.0679 · 0.0929 | 2.20 · 2 · 3 · 3 | 3.3 · 3.0 · 5.9 · 5.9 | 1.00 |
| todas | 0.1151 · 0.1552 | 3.68 · 4 · 5 · 8 | 8.5 · 8.5 · 14.4 · 17.4 | 1.00 |

En frío, promedio por consulta: GET .idx 2.68 · .db 0.00 · p.log 1.00 · otros 0.00; KB .idx 8.3 · .db 0.0 · p.log 0.2 · otros 0.0.

## A → B: qué cambian los grupos

| Consultas | N | Δ S@1 | Δ MRR | Δ nDCG@10 | Δ falsos | Δ $ por 1M (prom.) | Δ GET frío (prom.) | Δ KB frío (prom.) |
|---|---|---|---|---|---|---|---|---|
| consultas de nombre (sin grupo) | 185 | +0.00 | +0.00 | +0.00 | +0.00 | +0.0008 | +0.00 | +1.4 |
| nombre+grupo: sin grupo (A) → con grupo (B) | 25 | +0.24 | +0.32 | +0.23 | -0.01 | +0.0013 | +0.00 | +2.2 |

## Consultas donde perdemos contra R1 por más de 0,2 de nDCG (11)

- A/q021 `pilas especiales litio` (exacta): nDCG 0.78 contra R1 1.00
- A/q159 `madera bazar y accesorios` (nombre+grupo): nDCG 0.74 contra R1 0.98
- B/q021 `pilas especiales litio` (exacta): nDCG 0.78 contra R1 1.00
- B/q024 `ecológica eudermin` (exacta): nDCG 0.77 contra R1 1.00
- B/q156 `baby geles y jabones` (nombre+grupo): nDCG 0.68 contra R1 1.00
- B/q164 `ecológica cervezas` (nombre+grupo): nDCG 0.77 contra R1 1.00
- B/q176 `platos preparados preparados y ahumados` (grupo): nDCG 0.00 contra R1 0.97
- B/q181 `vajilla y cubiertos bazar` (grupo): nDCG 0.36 contra R1 0.89
- B/q183 `navideños dulces y desayunos` (grupo): nDCG 0.38 contra R1 1.00
- B/q188 `platos preparados` (grupo): nDCG 0.47 contra R1 0.95
- B/q190 `salud e higiene` (grupo): nDCG 0.57 contra R1 1.00
