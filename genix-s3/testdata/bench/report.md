# Benchmark de genix-s3

IDs: row numbers. Corpus: 10010 productos, 225 consultas. Se genera con `GXS_BENCH=1 go test ./internal/query -run Bench -v`.

Efectividad contra los juicios de relevancia (qrels): S@1, S@10 y MRR sobre las consultas con objetivo; nDCG@10 y P@10 sobre todas salvo las negativas; falsos es la fracción del top 10 con nota 0 (en las negativas, si devolvió algo). R1 es BM25 (FTS5), R2 BM25 de trigramas, R3 nuestras reglas sin índice. El solapamiento con R1 es parecido, no calidad.

## Base A: nombres

Índice (es100): .idx 160 KB · .db 4 KB · tokens 379.4 KB · p.log 0 KB · 103 objetos · build 43 ms, 107 PUTs, 921 KB escritos.

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

### Costo (solo genix-s3)

| Categoría | GET frío p50 · p95 · máx | KB frío p50 · p95 · máx | GET caliente | KB caliente | Rondas frío |
|---|---|---|---|---|---|
| exacta | 4 · 4 · 5 | 6.5 · 10.3 · 10.6 | 1.06 | 0.2 | 1.06 |
| parcial | 4 · 4 · 5 | 7.6 · 10.2 · 10.7 | 1.05 | 0.2 | 1.05 |
| escribiendo | 4 · 4 · 5 | 6.2 · 9.3 · 9.5 | 1.08 | 0.2 | 1.08 |
| typo | 4 · 4 · 5 | 7.2 · 10.5 · 11.4 | 1.04 | 0.2 | 1.04 |
| tildes | 4 · 5 · 5 | 7.4 · 11.0 · 11.0 | 1.20 | 0.4 | 1.20 |
| marca | 3 · 5 · 5 | 7.0 · 11.1 · 11.1 | 1.07 | 0.3 | 1.07 |
| numeros | 5 · 8 · 8 | 7.9 · 9.8 · 9.8 | 1.07 | 0.2 | 1.07 |
| plural | 4 · 5 · 5 | 6.7 · 11.0 · 11.3 | 1.08 | 0.2 | 1.08 |
| nombre+grupo | 4 · 5 · 6 | 7.0 · 11.8 · 14.3 | 1.16 | 0.3 | 1.16 |
| negativa | 2 · 3 · 3 | 2.7 · 4.2 · 4.2 | 1.00 | 0.2 | 1.00 |
| todas | 4 · 5 · 8 | 6.7 · 10.5 · 14.3 | 1.08 | 0.2 | 1.08 |

En frío, promedio por consulta: GET .idx 2.68 · .db 0.08 · p.log 1.00 · otros 0.00; KB .idx 6.5 · .db 0.1 · p.log 0.2 · otros 0.0.

## Base B: nombres + grupos (categoría)

Índice (es100): .idx 183 KB · .db 9 KB · tokens 515.9 KB · p.log 0 KB · 105 objetos · build 53 ms, 109 PUTs, 1223 KB escritos.

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

### Costo (solo genix-s3)

| Categoría | GET frío p50 · p95 · máx | KB frío p50 · p95 · máx | GET caliente | KB caliente | Rondas frío |
|---|---|---|---|---|---|
| exacta | 4 · 5 · 6 | 7.5 · 11.4 · 11.6 | 1.11 | 0.3 | 1.09 |
| parcial | 4 · 5 · 5 | 8.6 · 11.6 · 11.6 | 1.15 | 0.3 | 1.15 |
| escribiendo | 4 · 5 · 5 | 7.6 · 10.2 · 11.0 | 1.24 | 0.3 | 1.24 |
| typo | 4 · 4 · 5 | 8.2 · 11.7 · 15.7 | 1.04 | 0.3 | 1.04 |
| tildes | 4 · 5 · 5 | 8.5 · 12.9 · 12.9 | 1.27 | 0.5 | 1.27 |
| marca | 3 · 5 · 5 | 7.8 · 12.2 · 12.2 | 1.13 | 0.3 | 1.13 |
| numeros | 5 · 8 · 8 | 9.1 · 10.6 · 10.6 | 1.07 | 0.4 | 1.07 |
| plural | 4 · 5 · 5 | 8.4 · 12.0 · 12.4 | 1.20 | 0.2 | 1.20 |
| nombre+grupo | 4 · 5 · 6 | 8.8 · 14.8 · 17.1 | 1.36 | 0.5 | 1.36 |
| grupo | 4 · 5 · 5 | 7.5 · 12.3 · 12.3 | 1.13 | 0.4 | 1.13 |
| negativa | 2 · 3 · 3 | 3.0 · 5.1 · 5.1 | 1.00 | 0.2 | 1.00 |
| todas | 4 · 5 · 8 | 7.9 · 11.6 · 17.1 | 1.16 | 0.3 | 1.16 |

En frío, promedio por consulta: GET .idx 2.68 · .db 0.16 · p.log 1.00 · otros 0.00; KB .idx 7.5 · .db 0.1 · p.log 0.2 · otros 0.0.

## A → B: qué cambian los grupos

| Consultas | N | Δ S@1 | Δ MRR | Δ nDCG@10 | Δ falsos | Δ GET frío (p50) | Δ KB frío (p50) | Δ KB frío (prom.) |
|---|---|---|---|---|---|---|---|---|
| consultas de nombre (sin grupo) | 185 | +0.00 | +0.00 | +0.00 | +0.00 | +0 | +1.2 | +1.0 |
| nombre+grupo: sin grupo (A) → con grupo (B) | 25 | +0.24 | +0.32 | +0.23 | -0.01 | +0 | +1.8 | +1.6 |

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
