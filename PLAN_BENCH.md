# PLAN_BENCH: benchmark estándar de genix-s3

> Estado: **implementado** (2026-10-05); lo que se decidió al implementar, el resultado y la fase 5 están en 11 a 13. Creado el 2026-10-05. Un benchmark fijo que dice, para cada cambio del algoritmo, **qué tan bien encuentra** (efectividad) y **cuánto lee de S3** (eficiencia), comparado con BM25 y con versiones anteriores. Reemplaza al eval de `PLAN_BUCKETS.md` 13 como medida principal; el eval sigue para escalas grandes (100k) que este corpus no tiene.

## 1. Por qué

Los tests de hoy comparan el algoritmo consigo mismo:

- los tests de comportamiento (`TestTextGroups`, `TestLetters`…) tienen el orden esperado escrito con las mismas reglas que prueban;
- `TestMatchesBruteForce` y "top-10 vs the same rules" verifican que el índice no pierde nada respecto de sus reglas: es fidelidad, no calidad;
- "top-10 vs full words" mide el corte de 8 letras, pero con el mismo puntaje;
- las consultas del eval son 2–3 palabras seguidas de un nombre, sin un documento correcto conocido.

Solo los typos y las consultas de grupo tienen un documento objetivo. Falta una **colección de prueba**: corpus, consultas y juicios de relevancia fijos, y una referencia externa.

## 2. Corpus

`/home/ivanjoz/Downloads/download6/productos.xlsx`, hoja `Productos`:

- **10 010 productos** (columna `Producto`): son los nombres. 9 593 coinciden con `scripts/perf/data/productos.csv`, el corpus del eval.
- **315 categorías** (columna `Categorías`): son los grupos. Una por producto, como pide 17.
  - Por palabras (sin stopwords): 54 de 1, 102 de 2, 123 de 3, 31 de 4, 5 de 5.
  - Tamaño: mediana 4 productos, máximo 312 ("Quesos"); 165 tienen menos de 5.
  - Algunas juntan varias con comas ("Aceite, vinagre y sal, Lácteos"). Se usa el texto entero como grupo. La firma toma sus primeras 4 palabras.
- El 18 % de los productos comparte una palabra con su categoría ("Aceite de girasol…" en "Aceite"). Es un caso a vigilar: el término ya coincide por el nombre, así que no debe sumar de nuevo por la palabra de grupo.
- La columna `ID` está vacía. **ID = número de fila (1…10 010)**, origen 0. Con `-random-ids` se usan `uint32` al azar con semilla fija, porque los IDs aleatorios cuestan más (`PLAN_BUCKETS.md` 13).
- `Marca` (2 249 marcas, el 93 % la tiene) no se indexa: ya suele estar en el nombre. Solo se usa para generar consultas (4).

`scripts/bench/prepare.py` lo convierte una vez en `genix-s3/testdata/bench/productos.tsv` (`id`, `nombre`, `categoría`, `marca`), que se versiona. Así el benchmark no depende del xlsx ni de openpyxl.

## 3. Las dos bases locales

Cada base existe en dos motores: el nuestro y SQLite (la referencia).

| Base | Qué tiene | genix-s3 | SQLite FTS5 (referencia) |
|---|---|---|---|
| **A: nombres** | solo el nombre | `bench/db/names/`: `blob.FS`, `Doc{ID, Text}` | `bench/db/names.sqlite` |
| **B: grupos** | nombre + categoría como grupo | `bench/db/groups/`: `Doc{ID, Text, Group}` | `bench/db/groups.sqlite`: columnas `nombre` y `categoria` |

- **genix-s3:** se construye con `Build` sobre `blob.FS` y la agrupación por defecto (`es100`). Los directorios quedan en disco (en `.gitignore`), así que también se pueden consultar a mano con `gxs query -root bench/db/groups …`. El test los reconstruye si faltan o si cambió el formato.
- **SQLite:** lo crea `scripts/bench/reference.py` con el `sqlite3` de Python (3.51, trae FTS5 y el tokenizer trigram). El test de Go **no depende de SQLite**: lee los top 10 ya calculados (5). Así `go.mod` no cambia.

Las consultas de nombre se corren en las dos bases. La diferencia A → B mide cuánto cuestan los grupos en consultas que no los nombran, y cuánto ayudan en las que sí.

## 4. Las 200 consultas

`scripts/bench/queries.py` las genera con semilla fija a partir de productos al azar. **Yo las reviso a mano una vez** y quedan congeladas en `testdata/bench/queries.tsv` (`id`, `categoría`, `consulta`, `objetivo`, `bases`). Una consulta no se regenera: si cambia, cambia su id.

| Categoría | N | Cómo se arma | Ejemplo (de "Aceite de girasol Koipesol 1 l.") | Objetivo |
|---|---|---|---|---|
| `exacta` | 35 | 2–3 palabras seguidas del nombre | `aceite girasol koipesol` | el producto |
| `parcial` | 20 | 2–3 palabras salteadas o desordenadas | `koipesol girasol` | el producto |
| `escribiendo` | 25 | la última palabra cortada a 3–5 letras | `aceite girasol koip` | el producto |
| `typo` | 25 | una edición en una palabra de 5+ letras; 1 de cada 4 en las 2 primeras letras | `aceite girsaol koipesol` | el producto |
| `tildes` | 15 | sin tildes, con tildes de más, mayúsculas | `AZUCAR moreno` | el producto |
| `marca` | 15 | marca + tipo de producto | `hacendado aceite oliva` | los de esa marca y tipo |
| `numeros` | 15 | con cantidad o unidad (`5 l`, `1 kg`, `0,4`) | `aceite girasol 5 l` | el producto |
| `nombre+grupo` | 25 | 1–2 palabras del nombre + la categoría (solo B) | `girasol aceite vinagre sal` | el producto |
| `grupo` | 15 | solo la categoría (solo B) | `vinos tintos` | los miembros |
| `negativa` | 10 | palabras que no están en el corpus, o que comparten las 2 primeras letras con una categoría | `producto premium`, `smartphone` | ninguno |

En `nombre+grupo`, las palabras del nombre se eligen para que **solas sean ambiguas** (más de 10 productos las tienen): así se ve si el grupo desempata. Las de `marca` y `grupo` no tienen un objetivo único; se miden solo con los juicios (5).

Las 160 consultas que no son de grupo se corren en A y en B; las 40 de grupo, en B. Además, las 25 `nombre+grupo` se corren en A, como "sin grupo". (Antes decía 165: 200 − 40 son 160.) En total son 385 pares consulta-base.

## 5. Referencias y juicios de relevancia

### 5.1 Los top 10 de referencia

`reference.py` guarda los top 10 de cada consulta en `testdata/bench/reference.tsv`:

| Sistema | Qué es | Para qué |
|---|---|---|
| **R1: BM25** | FTS5, tokenizer `unicode61 remove_diacritics 2`. Las palabras en OR (como Lucene); la última también como prefijo (`word*`), igual que la nuestra. En B, `bm25(t, 1.0, 0.3)`: la categoría pesa menos que el nombre | la referencia de la industria |
| **R2: BM25 trigram** | FTS5 con tokenizer `trigram` sobre el mismo texto | tolera typos y palabras cortadas; referencia de "fuzzy" |
| **R3: nuestras reglas sin índice** | el mismo puntaje sobre los 10 010 documentos, palabras completas, sin presupuesto (`reference`/`fullText` de los tests, calculado en Go) | la cota superior de nuestras reglas |

**El top 10 de BM25 no es la verdad.** BM25 no tiene typos ni "escribiendo" de verdad, y premia palabras raras aunque falte la importante. Si diferimos de BM25 no significa que estemos mal. Por eso coincidir con BM25 se informa como **parecido** (solapamiento), y la **calidad** se mide con juicios independientes de cualquier motor.

### 5.2 Juicios (qrels)

`judge.py` califica **todos** los documentos para cada consulta con una regla sobre el texto completo, sin corte de 8 letras. La normalización es la misma que la nuestra: minúsculas, sin tildes, sin stopwords.

| Nota | Cuándo |
|---|---|
| 3 | el objetivo, o un documento con el mismo nombre normalizado (hay repetidos) |
| 2 | el nombre tiene todas las palabras de la consulta: completas, la última como prefijo en `escribiendo`, la palabra correcta en `typo`. En B, las de grupo pueden estar en la categoría. En `grupo`: es miembro de la categoría. En `marca`: tiene la marca y el tipo |
| 1 | tiene alguna palabra de la consulta, pero no todas |
| 0 | ninguna |

- Se guardan en `testdata/bench/qrels.tsv` (`consulta`, `id`, `nota`), solo con nota > 0.
- Como la regla califica todo el corpus, el nDCG ideal se calcula sin pooling, y un cambio del algoritmo no obliga a juzgar de nuevo.
- **Validación:** reviso a mano 30 consultas (las del pool R1 ∪ R2 ∪ nuestro top 10) y anoto dónde la regla se equivoca. Si se equivoca en más del 5 %, se corrige la regla antes de congelar.

## 6. Métricas

### Efectividad, por consulta y promedio por categoría y base

| Métrica | Qué mide | Sobre |
|---|---|---|
| **Success@1** | el objetivo (nota 3) queda primero | consultas con objetivo |
| **Success@10** | el objetivo está en el top 10 | consultas con objetivo |
| **MRR** | 1/lugar del objetivo (0 si no está en el top 10): premia quedar arriba | consultas con objetivo |
| **nDCG@10** | el orden del top 10 contra el ideal según las notas | todas, salvo negativas |
| **P@10** | fracción del top 10 con nota ≥ 2 | todas, salvo negativas |
| **Falsos** | fracción del top 10 con nota 0; en negativas, si devolvió algo | todas |
| **Solapamiento con R1** | \|top 10 nuestro ∩ top 10 BM25\| / 10 | todas: parecido, no calidad |

Se calculan igual para R1, R2 y R3, para poder compararlos en la misma tabla.

### Eficiencia: solo nuestro motor

Cada consulta se cuenta con `blob.Counting` sobre `blob.FS`, con los mismos `Stats` del eval:

- **GET requests** por consulta: total, y separados en `.idx`, `.db` y `p.log`.
- **KB leídos** por consulta: total, `.idx`, `.db`, `p.log`.
- **Rondas** (requests en serie).
- **En frío:** un engine nuevo por consulta, como una Lambda que arranca.
- **En caliente:** la misma consulta otra vez, con los `.idx` en memoria.
- Se informan promedio, p50, p95 y máximo, por categoría y base.
- Por base, una vez: tamaño del índice (`.idx`, `.db`, `tokens`), PUTs y tiempo de build.

## 7. Ejecución y reporte

```sh
python3 scripts/bench/prepare.py ~/Downloads/download6/productos.xlsx   # → testdata/bench/productos.tsv
python3 scripts/bench/queries.py      # → queries.tsv (se revisa a mano y se congela)
python3 scripts/bench/queries_plural.py  # → agrega q201–q225, categoría plural (14)
python3 scripts/bench/reference.py    # → bench/db/*.sqlite, reference.tsv (R1, R2)
python3 scripts/bench/judge.py        # → qrels.tsv
cd genix-s3 && GXS_BENCH=1 go test ./internal/query -run Bench -v
```

Los tres archivos de referencia (`queries.tsv`, `reference.tsv`, `qrels.tsv`) se versionan. Los scripts solo se corren de nuevo si cambia el corpus o una consulta.

`TestBench` (en `internal/query/bench_test.go`, salta sin `GXS_BENCH`):

1. construye o abre `bench/db/names` y `bench/db/groups`;
2. corre las consultas en frío y en caliente, y calcula R3;
3. escribe `testdata/bench/report.md` (las tablas de 8) y `results.json` (todo, por consulta);
4. compara con `testdata/bench/baseline.json`, el último resultado aceptado. **Falla** si:
   - el nDCG@10 o el MRR global bajan más de 0,01;
   - alguna categoría baja más de 0,03;
   - los GET o KB p50 en frío suben más de un 5 %.

   Se acepta un cambio copiando `results.json` a `baseline.json` en el mismo commit, y el diff muestra qué consultas cambiaron.

También lista las consultas donde perdemos contra R1 por más de 0,2 de nDCG: son los casos a mirar.

## 8. El reporte

Una tabla de efectividad por base, con categorías en filas:

| Categoría | N | S@1 · S@10 · MRR | nDCG@10 · P@10 · falsos | R1 nDCG | R2 nDCG | R3 nDCG | Solapamiento R1 |
|---|---|---|---|---|---|---|---|

Una tabla de costo por base:

| Categoría | GET frío p50 · p95 · máx | KB frío p50 · p95 · máx | GET caliente | KB caliente | Rondas |
|---|---|---|---|---|---|

Y una fila "A → B" con el cambio de efectividad y costo de las consultas de nombre al agregar grupos.

## 9. Fases

1. `prepare.py`, `queries.py` y la revisión de las 200 consultas.
2. `reference.py` (R1, R2) y `judge.py`; validación de la regla con 30 consultas.
3. `bench_test.go`: bases, consultas, métricas, `report.md`, `results.json`.
4. Primer `baseline.json` y una sección en `PLAN_BUCKETS.md` con el resultado.
5. Usarlo en las decisiones pendientes:
   - firma de 1ª+3ª letra contra 1ª+2ª (17);
   - los valores de las constantes de puntaje;
   - el presupuesto de prefijos.

## 10. Riesgos y decisiones abiertas

- **La regla de juicio es léxica.** Favorece a los motores léxicos, que somos los tres. No juzga sinónimos ("gaseosa" por "refresco"), y ninguno de los tres los resuelve. Si se agregan, hacen falta juicios a mano.
- **La regla no ordena dentro de una nota.** Dos productos con todas las palabras valen igual, así que el nDCG no ve si preferimos el más corto o el que empieza por la palabra. Para eso están Success@1 y MRR en las consultas con objetivo.
- **10k productos es escala chica.** El costo a 100k sigue midiéndose con el eval (×10, IDs aleatorios).
- **`ñ` y la normalización:** FTS5 con `remove_diacritics 2` convierte `ñ` en `n`. El juez usa nuestra normalización. Hay que revisar que las consultas con `ñ` no castiguen a R1 por eso.
- **Categorías con comas:** el texto entero es un grupo. Si en la aplicación real cada producto tiene una sola categoría, se toma la primera. Hay que decidirlo antes de la fase 1. *(Decidido al implementar: el texto entero; ver 11.)*

## 11. Implementación (2026-10-05): lo que se decidió

Las cinco fases están hechas. Todo lo siguiente lo decidí yo al implementar, sin poder preguntar; está para que lo revises.

**Archivos**

- `scripts/bench/`: `common.py` (el corpus, un port de `normalize` para que el juez lea el texto como el motor, el índice invertido), `prepare.py`, `queries.py`, `judge.py`, `reference.py`.
- `genix-s3/testdata/bench/`: `productos.tsv`, `queries.tsv`, `qrels.tsv`, `reference.tsv`, `baseline.json`, y lo que escribe cada corrida: `report.md`, `results.json`. Los tres archivos de referencia y el baseline se versionan.
- `genix-s3/internal/query/bench_suite_test.go`: `TestBench`. No quedó en `bench_test.go` porque ese archivo ya tenía los `Benchmark*` de CPU.
- `genix-s3/bench/db/` (en `.gitignore`): `names/`, `groups/` (con `gxs query -root bench/db/groups -collection productos …`) y `names.sqlite`, `groups.sqlite`.

```sh
GXS_BENCH=1 go test ./internal/query -run 'TestBench$' -v                # ~5 s
GXS_BENCH=1 go test ./internal/query -run 'TestBench$' -v -random-ids    # IDs uint32 al azar
```

Con `-random-ids` los resultados van a `*.random-ids.*` (fuera de git) y solo se comparan con un `baseline.random-ids.json` si existe.

**Corpus y consultas**

- Las categorías con comas son un solo grupo (el texto entero), como decía el plan. 10 010 productos, 315 categorías, 2 248 marcas.
- Revisé las 200 consultas a mano una vez y las dejé como salieron. Rarezas aceptadas: `dó cigales` y `quesó vaca ríe` (tildes de más, a propósito); las 5 negativas con palabras inventadas (`lazkcte`: empiezan con las 2 letras de una categoría y no existen en el corpus); `marca` y `grupo` no tienen objetivo.
- Las consultas se generan con la condición de que el objetivo sea identificable: las de nombre tienen a lo más 5 documentos con todas sus palabras (10 en `escribiendo`); las `nombre+grupo`, más de 10 con las palabras del nombre solas y entre 1 y 4 con nombre y grupo; las `marca`, entre 3 y 60 documentos de esa marca y tipo.
- Solo las consultas `typo` piden `Fuzzy: true`; las demás no.

**Juez (`judge.py`)**

- La nota no depende de la base: las consultas de grupo se juzgan con la categoría, se corran donde se corran.
- `typo`: la palabra corregida se deduce del objetivo (la única palabra de la consulta que no está en su nombre, a una edición de una que sí). Si no es única, el generador la descarta.
- La nota 1 no cuenta números ni palabras de una letra: "5 l" no hace "relevante" a medio corpus.
- `marca` se juzga por el nombre, no por la columna `Marca`, que ningún motor indexa (la marca ya suele estar en el nombre, como dice 2).
- `grupo`: nota 2 = los documentos cuya categoría tiene exactamente las palabras de la consulta.
- El objetivo es cualquier documento de nota 3 (el mismo nombre normalizado), no solo el id elegido.

**Referencias**

- R1 y R2 quitan las stopwords de la consulta (como un analizador de Lucene). Empates por id.
- R2 es la OR de los trigramas de la consulta (`remove_diacritics 1`).
- R3 es `reference` (nuestras reglas, corte de 8 letras, sobre todos los documentos, sin presupuesto) en las dos bases, porque `fullText` no conoce los grupos. En A se agrega una fila "R3 con palabras completas".
- Es la misma lista que la nuestra en las 385 consultas (R3 = genix-s3 en el reporte): el índice no pierde nada respecto de sus reglas. Si algún día difieren, `TestEval` lo dice mejor.

**Métricas y el gate**

- P@10 divide por 10 aunque el sistema devuelva menos; "falsos" divide por lo devuelto (0 si no devolvió nada). Como la mayoría de las consultas tiene menos de 10 documentos de nota ≥ 2, el P@10 máximo posible es bajo (0,2): sirve para comparar, no como valor absoluto.
- El gate compara por base: nDCG@10 y MRR de "todas" (−0,01) y de cada categoría (−0,03); GET y KB en frío p50 de "todas" (+5 %). El p50 por categoría no se mira: con 10–25 consultas, una más o menos mueve un 25 %.
- `results.json` es determinista: sin tiempos (el de build va solo en `report.md` y en `stamp.json`), una consulta por línea para que el diff del baseline muestre qué cambió. Un cambio de código que altera el índice reconstruye las bases solo (el `stamp.json` guarda el hash del corpus y del código de los paquetes que escriben el índice, no una versión de formato).

**Validación del juez (fase 2)**

Revisé a mano 29 consultas (3 a 4 por categoría), con el pool R1 ∪ R2 ∪ nuestro top 10, unos 500 documentos. No encontré una nota equivocada. No revisé las ~73 000 notas de la cola, y la regla es léxica (10). Las 10 negativas las verifica `judge.py`: ninguna palabra existe en el corpus, ni como palabra ni como prefijo ni como palabra de grupo.

## 12. Resultado inicial

IDs = número de fila, 10 010 documentos, `es100`. Detalle en `genix-s3/testdata/bench/report.md`.

| | S@1 | S@10 | MRR | nDCG@10 | P@10 | falsos |
|---|---|---|---|---|---|---|
| A: genix-s3 | 0,72 | 0,90 | 0,78 | 0,90 | 0,20 | 0,03 |
| A: R1 BM25 | 0,63 | 0,87 | 0,72 | 0,86 | 0,19 | 0,03 |
| A: R2 trigramas | 0,59 | 0,90 | 0,71 | 0,82 | 0,20 | 0,22 |
| B: genix-s3 | 0,78 | 0,97 | 0,85 | 0,92 | 0,22 | 0,03 |
| B: R1 BM25 | 0,72 | 0,96 | 0,81 | 0,91 | 0,23 | 0,02 |
| B: R2 trigramas | 0,69 | 0,96 | 0,79 | 0,85 | 0,23 | 0,20 |

(A incluye las 25 `nombre+grupo` sin su grupo, que ahí no se pueden encontrar: por eso su S@1 es menor.)

Costo en frío, por consulta (promedio): A 3,8 GET y 6,7 KB; B 3,9 GET y 7,8 KB (p50 4 GET en las dos). Las rondas son 1,1 en frío. Un GET en caliente (p.log). Por tipo: ~2,7 `.idx`, 1 `p.log`, 0,1 `.db`. Los grupos cuestan en consultas que no los nombran +1,1 KB (p50) y 0 GET.

**Lo que muestra, para mirar**

1. **Efectividad:** igualamos a BM25 (±0,01 de nDCG) en `exacta`, `parcial` y `tildes`, y lo superamos en `escribiendo` (0,98 contra 0,95), `typo` (0,87 contra 0,75), `numeros` (0,96 contra 0,88) y `marca` (1,00 contra 0,93). R2 tolera typos pero devuelve basura (falsos 0,22; en las 10 `negativa` devuelve algo).
2. **Los typos en las 2 primeras letras no se corrigen, por diseño** (6 de las 25 `typo`): S@1 0,17, S@10 0,67 y MRR 0,27, contra 0,89, 1,00 y 0,95 de los 19 restantes. Los que se encuentran es por la otra palabra de la consulta. Es el costo de buscar solo en el bucket de la palabra.
3. **Consultas que son solo el nombre de la categoría (`grupo`)**: nDCG 0,60 contra 0,80 de R1. Por diseño el nombre pesa más que el grupo: "platos preparados" devuelve "Preparado de fabada…" (nombre) antes que los 146 miembros de la categoría. Si esa consulta es un caso de uso real, el grupo no alcanza; si no, es el comportamiento pedido.
4. **`nombre+grupo`:** S@1 0,16 sin grupo → 0,52 con grupo (S@10 0,44 → 0,92). Es lo que el grupo promete, y queda por debajo de R1 en nDCG (0,87 contra 0,91): R1 pesa la categoría 0,3 y suma ahí las palabras.
5. **IDs aleatorios cuestan:** con `-random-ids` el `.db` pasa de 4 KB a 395 KB, y el costo en frío de 3,8 a 4,6 GET y de 6,7 a 11,5 KB (A). La efectividad no cambia.
6. **Presupuesto de prefijos:** a esta escala no importa (11, fase 5).

## 13. Fase 5: las decisiones pendientes

Cada variante se corrió sobre una copia del código en `/tmp` (`TestBench` completo, IDs de fila y al azar). Ningún cambio de estas constantes altera el costo (GET y KB idénticos): solo el orden.

**Firma de la 2.ª palabra en adelante: 1.ª+3.ª letra (la actual) contra 1.ª+2.ª.** Con 1.ª+2.ª los falsos suben de 0,033 a 0,041 y una consulta negativa empieza a devolver documentos, sin ganar nada en efectividad ni costo. **Se queda 1.ª+3.ª.** (La diferencia es de una consulta: poca evidencia, pero en una sola dirección.)

**Presupuesto de prefijos (`PrefixBudget`, 32 KB).** Con 4 KB, 8 KB, 128 KB y sin tope, los resultados son idénticos en las dos modalidades de IDs; recién con 0–1 byte baja el nDCG (0,900 → 0,896). En 10k documentos el presupuesto nunca llega a cortar. **Se queda en 32 KB;** esto solo se decide con el eval a 100k o más.

**Constantes de puntaje** (nDCG global de B, base 0,910; una consulta de S@1 son 0,005):

| Variante | nDCG B | S@1 `nombre+grupo` | Se aplica |
|---|---|---|---|
| base | 0,910 | 0,44 | |
| `IDFMax` 15 → 8 (igual con 5) | 0,917 | 0,52 | **sí** |
| `IDFWeight` 0 | 0,917 | 0,52 | no: el tope ya lo logra |
| `IDFWeight` 4 con `IDFMax` 15 | 0,918 | 0,52 | no: es lo mismo que bajar el tope |
| `PairNear` 2 → 4 | 0,911 | 0,44 | no: dentro del ruido |
| `DocSecondWord` 1 → 0, con lo anterior | 0,919 | 0,56 | no: dentro del ruido |
| `GroupSegment` 4 → 6 | 0,917 | 0,48 | no: es decisión tuya |
| `GroupFull` 2 → 4 | 0,915 | 0,48 | no: es decisión tuya |
| `GroupWord` 4 → 6 | 0,913 | 0,48 | no: es decisión tuya |
| `LetterNext` 3 → 1 | 0,916 | 0,44 | no: es decisión tuya |
| `WordFull` 7 → 12 | 0,912 | 0,44 | no: es decisión tuya |
| `PairAdjacent` 6 → 2 / `PairAdjacentReversed` 3 → 6 | 0,907 / 0,908 | 0,44 | no: peor |

**Aplicado: `IDFMax` 15 → 8.** Mejora B (nDCG 0,910 → 0,917; S@1 de `nombre+grupo` 0,44 → 0,52) y A (0,897 → 0,899), con IDs de fila y aleatorios, y no cambia el costo. El IDF es `round(2·ln(N/df))`: con tope 8 toda palabra en menos del 1,8 % de los documentos ya cuenta como rara, y el puntaje deja de premiar al documento que tiene una palabra muy rara y le falta otra. El tope es una fracción del corpus, no un número de documentos, así que no depende del tamaño. Pasan todos los tests. Si lo prefieres, es una constante en `query/score.go`.

**No toqué** las constantes que me diste (`LetterFirst` 5, `LetterNext` 3, `WordFull` 7, `GroupSegment` 4, `GroupFull` 2, `GroupWord` 4), aunque subir los de grupo ayuda a `nombre+grupo` (S@1 hasta 0,48): con 25 consultas, la diferencia son 1 a 2 consultas.

**Lo que no se puede concluir con 200 consultas:** todas las diferencias de arriba son de 1 a 3 consultas. Sirven para descartar cambios que empeoran, no para afinar. Para afinar las constantes de grupo hace falta un conjunto de consultas de grupo más grande.

## 14. Singular y plural (2026-10-05)

La revisión a mano de las 20 peores consultas mostró que el benchmark no podía ver un problema: las consultas se arman con las palabras del nombre del objetivo, así que ninguna escribía "bolsas" donde el nombre dice "Bolsa" (0 de 330 palabras). Cambios:

- **Categoría `plural`, 25 consultas (q201–q225)**, de `scripts/bench/queries_plural.py`, que solo reescribe esas filas: las 200 de antes no cambian. Como `exacta`, con una palabra en el otro número: 13 escriben el plural de un singular ("masa empanadas") y 12 al revés ("taco pota"). La otra forma tiene que existir en el corpus, y no se pluralizan marcas, para que sean consultas que alguien escribiría. Se corren en A y en B, sin `Fuzzy`. En total son 225 consultas y 435 pares consulta-base.
- **El juez acepta singular y plural en todas las categorías** (`forms` en `judge.py`): una palabra de la consulta también se cumple con su singular o plural, si el corpus lo tiene ("filetes" con "Filete de panga"). Salvo en `grupo` (la categoría se compara tal cual) y en la palabra cortada de `escribiendo` (ya es un prefijo). Antes, un documento con "Filete" para "filetes" valía 0 y contaba como falso. Las notas cambiaron, así que **los números de 12 y 13 no se comparan con los de ahora**.
- La fila "R3 con palabras completas" aplica la misma regla de palabras cercanas.

Con esto se implementaron las **palabras cercanas** (`PLAN_BUCKETS.md` 19): "bolsas" encuentra "Bolsa", con una letra de diferencia o con "es". Resultado (IDs de fila):

| | A: S@1 · MRR · nDCG | B: S@1 · MRR · nDCG | `plural` (B): S@1 · nDCG | falsos A · B | GET · KB frío (B) |
|---|---|---|---|---|---|
| antes | 0,70 · 0,77 · 0,897 | 0,75 · 0,83 · 0,912 | 0,56 · 0,85 | 0,018 · 0,022 | 3,83 · 7,8 |
| después | 0,75 · 0,82 · 0,926 | 0,78 · 0,86 · 0,929 | 0,80 · 0,98 | 0,018 · 0,024 | 3,84 · 7,8 |
| R1 BM25 | 0,61 · 0,71 · 0,855 | 0,69 · 0,79 · 0,895 | 0,48 · 0,81 | | |

Sube todo y no cuesta requests ni KB. Con 2 letras cualesquiera (no solo "es") los falsos suben a 0,023 · 0,029 sin ganar nada; con 1 letra sola, `plural` queda en 0,76. `baseline.json` es este resultado.

## 15. Grupo contado una vez y palabras repetidas (2026-10-05)

Dos fallas de la revisión de las 20 peores (`PLAN_BUCKETS.md` 17.5):

- **Palabras repetidas:** `Parse` deja un solo término por palabra ("platos preparados preparados y ahumados"). No cambia ninguna consulta del benchmark: q176 está abajo por otra razón (ver abajo).
- **Palabras del grupo que también suman por el nombre:** en "creme cuidado corporal", "X corporal @ cuidado corporal" ganaba "corporal" por el nombre y por la firma. Se probaron tres reglas (base B, nDCG):

| Regla | todas | `nombre+grupo` | `grupo` | q161 · q165 · q176 |
|---|---|---|---|---|
| antes (baseline de 14) | 0,929 | 0,877 | 0,599 | 0,38 · 0,52 · 0,24 |
| la corrida de la firma cuenta una vez, siempre | 0,92 | 0,87 | 0,534 | 0,38 · 0,56 · 0,24 |
| una vez si el nombre tiene solo parte de la corrida | 0,92 | 0,89 | 0,468 | 0,65 · 0,62 · 0,00 |
| **una vez si la consulta tiene palabras fuera de la corrida** | **0,929** | **0,888** | **0,590** | **0,65 · 0,62 · 0,00** |

Contar una vez siempre rompe las consultas de grupo: en "limpieza facial" o "ensaladas" lo que pone primero al miembro cuyo nombre también lo dice es justamente que nombre y grupo sumen los dos; contado una vez, empata con un producto de otra categoría que tiene la palabra en el nombre. Por eso la regla aceptada solo cuenta una vez cuando la consulta es un nombre más un grupo.

En total es neutra (B nDCG 0,929 igual; `nombre+grupo` +0,011, `grupo` −0,009, falsos B 0,024 → 0,026) y sin costo de requests ni KB: arregla los casos que la motivaron (q161, q165) y pierde q176. q176 no es por esta regla: sus 3 objetivos tienen la firma completa de 4 segmentos (18 puntos), y cualquier "Preparado …" de otra categoría ya suma 44 con una sola palabra cercana. Es el problema de fondo de que un grupo vale menos que una palabra del nombre (también q172), que se deja para decidir.

## 16. Primera palabra de la consulta (2026-10-05)

A pedido del usuario: la primera palabra que escribe suele guiar la intención ("cafe organico"). `DocFirstWord` baja de 3 a 2, y `QueryFirstWord` = 2 se suma a la palabra del nombre que coincide con la primera de la consulta (`PLAN_BUCKETS.md` 17.5).

| | A: S@1 · MRR · nDCG | B: S@1 · MRR · nDCG | B `nombre+grupo` nDCG | B `exacta` nDCG | falsos A · B |
|---|---|---|---|---|---|
| antes (15) | 0,746 · 0,816 · 0,926 | 0,778 · 0,859 · 0,929 | 0,888 | 0,973 | 0,018 · 0,026 |
| después | 0,746 · 0,816 · 0,926 | 0,778 · 0,859 · 0,930 | 0,893 | 0,971 | 0,018 · 0,026 |

Cambian 85 top 10, pero la calidad queda igual, y no cuesta requests ni KB (solo cambia el puntaje). El benchmark casi no puede ver esta regla: sus consultas se arman con palabras del nombre del objetivo, casi siempre en el orden del nombre, así que la primera palabra de la consulta ya suele ser la primera del producto. La regla queda por intención del usuario, no por el benchmark. `baseline.json` es este resultado.
