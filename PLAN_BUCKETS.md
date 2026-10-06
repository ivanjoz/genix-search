# Plan: índice por buckets (`.idx` + `.db`) sobre S3 Express

> Estado: **implementado** (2026-10-03) en `genix-s3`. Reemplaza las secciones 5 a 8 de `PLAN_5BIT.md` (formatos, ruta de consulta, escritura): el diccionario global `k.<gen>`, los postings `p.<gen>`, el snapshot, el manifest y las generaciones desaparecen. **Siguen vigentes** las secciones 2 a 4 de `PLAN_5BIT.md`: el alfabeto de 5 bits, la clave de 8 letras con su cola de 2 bits, los números `NUM_n`, el normalizador y `NormID`.
> **Qué se optimiza**, en orden: los requests a S3 por consulta, después los bytes leídos por consulta. El tamaño de los archivos no es el objetivo. No se supone nada del corpus ni de los IDs (`uint32` arbitrarios dentro de cada origen, 15).
> Lo que la implementación decidió por su cuenta, y lo que quedó para después (cabezas de prefijo, combinaciones de 3 palabras), está en 12. Las mediciones están en 13. La agrupación de buckets en menos objetos (flag `Grouping`; `es100` por defecto) está en 14. El origen del documento (`uint8`, varias tablas en un índice; antes "grupo del documento") está en 15. Las palabras de una letra (continuaciones) y el flag de stopwords están en 16. El grupo de texto del documento ("Juan Carlos" en "centro civico": firma y palabras de grupo) y el ranking por puntos están en 17. El benchmark estándar (corpus fijo, juicios de relevancia, BM25 de referencia) está en 18 y en `PLAN_BENCH.md`. Singular y plural (palabras cercanas, "bolsas" encuentra "Bolsa") están en 19.

## 0. Resumen

- Una partición es `p.log`, un par `<xy>.idx` / `<xy>.db` por cada par de primeras letras (bucket) que exista, y `tokens`, que solo lee la compactación.
- **El nombre del índice de una palabra sale de la consulta**: "camisa" está en `ca.idx`. Por eso la ronda 1 pide `p.log` y los `.idx` en paralelo, sin directorio, sin generación y sin manifest.
- **El `.idx` se vuelve más específico donde hay densidad:** tiene todas las palabras del bucket con la posición exacta de su lista, las listas chicas dentro del propio `.idx`, y para las palabras grandes una **cabeza** (sus mejores productos) y un directorio de **combinaciones con las otras palabras** (`camisa·roja`).
- **La ronda 2 lee solo rangos exactos del `.db`**: la lista de una palabra o un bloque de combinaciones. Muchas consultas no tienen ronda 2, porque lo que necesitan está en el `.idx`.

| Consulta en frío | GET | Rondas |
|---|---|---|
| 1 palabra con la lista o la cabeza en el `.idx` | 2 | 1 |
| 1 palabra con la lista en el `.db` | 3 | 2 |
| N palabras | 1 + buckets distintos + rangos del `.db` | 2 |
| Colección chica (sin índice) | 2 (el `.idx` da 404) | 1 |
| En caliente (`.idx` en caché) | 1 + rangos del `.db` | 1 |

Semántica de resultados: la de `PLAN_5BIT.md` 13.3, **O con niveles estrictos**: primero cuántas palabras de la consulta tiene el producto (cobertura), después el puntaje, después la clave del documento (origen, después ID; 15).

---

## 1. Objetos de una partición

```
<root><col>/<part>/p.log          cambios pendientes                    se agrega al final; la compactación lo reescribe (If-Match)
<root><col>/<part>/<xy>.idx       índice del bucket xy                  se sobrescribe cuando el bucket cambia
<root><col>/<part>/<xy>.db        listas del bucket xy (si hacen falta) se sobrescribe entero, antes que su .idx
<root><col>/<part>/tokens         palabras de cada producto             solo la compactación
<root><col>/<part>/compact.lease  exclusión entre compactaciones        If-None-Match, con vencimiento
```

- **Un solo `p.log` para todo el corpus de la partición**, compartido por todos los buckets: no hay log por bucket. `ca` tiene `ca.idx` y `ca.db`, y nada más. Un registro que se agrega antes de la compactación vive solo en `p.log`; los buckets de sus palabras no cambian hasta que se compacta, y toda consulta lee `p.log` en la ronda 1 y le aplica esas ops (6.1).
- **Nombre del bucket:** las dos primeras unidades de la clave, cada una con un carácter de `0abcdefghijklmnopqrstuvwxyz12345` (unidad 0 = `0`, 1–26 = `a`–`z`, 27–31 = `1`–`5`). `camisa` → `ca`; el número 50 (`NUM_2`, unidad alta 1) → `2a`. Una palabra de una letra no tiene bucket propio: es una continuación de las 2 palabras anteriores más cercanas, en el bucket de cada una (16). Por eso todo bucket de letras es una combinación de 2 letras.
- **Por defecto (`GroupingES100`, 14)** los objetos son por grupo de buckets contiguos: `ca.idx` sigue siendo solo `ca`, y `vo-y0.idx` tiene de `vo` a `y0`. Hay 100 grupos, así que como mucho 200 objetos en vez de 2 048. Con `GroupingNone` un objeto es un bucket. El resto de este documento habla de buckets: con agrupación, léase "el objeto del grupo del bucket".
- **`<xy>.db` solo existe si alguna lista del bucket no entra en el `.idx`.** Medido en `productos.csv` (10k), sobre 379 buckets: 3 con IDs densos y 42 con IDs aleatorios. A 100k: 126 y 172.
- Una colección con menos de `MinIndexDocs` documentos (100) no tiene `.idx`, `.db` ni `tokens`: todo está en `p.log` (`PLAN_5BIT.md` 5.8, sin cambios).

---

## 2. Piezas comunes

Reglas de `PLAN_5BIT.md` 5.0: solo bytes, little‑endian, streams de bits LSB‑first, CRC32C, `magic` + `Format` en cada header.

### 2.1 Lista de una palabra

Es el formato de `PLAN_5BIT.md` 5.2: n postings ordenados por `(clave, pos)`, con la clave del documento `origen << 32 | ID` (15). Un producto con la palabra en dos posiciones tiene dos postings.

```
diferencias de clave n − 1 valores: W bits fijos (Codec 0, con o sin escapes de origen, 15.2) o varint de nibbles (Codec 1: 3 bits de valor + 1 de continuación)
posiciones          n × 4 bits (saturadas en 15)
colas               n × 2 bits, solo si Tail = 1
```

El diccionario guarda `count`, la primera clave y `V` (`Tail | Codec << 1 | X << 2`, con X = `W | nibbles de escape << 6` o la cantidad de nibbles). Con eso el largo en bits se calcula, así que no se guarda.

### 2.2 Columnas de una combinación

Las listas de dos palabras sobre los mismos productos. Se leen enteras y de adelante hacia atrás, así que se delimitan solas:

```
docs          varint de nibbles    productos
first         varint de nibbles    primera clave − base del bloque
codec         1 bit                0: escapes (1 bit), W (6 bits) y W bits por diferencia; 1: varint de nibbles por diferencia
por columna:  dup 1 bit | tail 1 bit
              con dup: por producto, sus postings − 1 (nibbles)
              por posting: pos (4 bits); después, con tail, por posting: cola (2 bits)
```

### 2.3 Stream de claves

- **Claves de un bucket:** el formato de `PLAN_5BIT.md` 3.2 (cabecera de una unidad `(s, L)` y las letras nuevas, después de las dos del bucket).
- **Claves de cualquier bucket** (las de un directorio o bloque de combinaciones): por clave, `s` (3 bits: unidades compartidas con la anterior), `L − 1` (3 bits: unidades de la clave) y las `L − s` unidades nuevas de 5 bits.

---

## 3. `<xy>.idx`

```
Header (64 B)
  0   magic "GXI1"
  4   Format       uint16 = 3
  6   ETagLen      uint16     largo del ETag del .db
  8   NormID       uint32
  12  Epoch        uint32     época del índice (7)
  16  Seq          uint64     la compactación que lo escribió: incluye las ops ≤ Seq
  24  Group        uint16     el grupo (14); sin agrupación, el bucket
  26  Grouping     uint8      la agrupación con que se escribió (14): 0 = none, 1 = es100
  27  reservado    uint8
  28  Keys         uint32     claves del objeto (0 = vacío, una lápida)
  32  DBSize       uint32     0 = no hay .db
  36  DBCombos     uint32     byte donde empieza la zona de combinaciones del .db
  40  KeysLen, FieldsLen, HeadsLen, ExpLen, InlineLen   5 × uint32: largo de cada sección
  60  CRC32C       uint32     del header (0..60) y de todo lo que sigue
ETag     [ETagLen]B   ETag que S3 devolvió al escribir el .db
Keys     n u16 | por bucket del grupo que tiene claves: bucket u16 | claves u32
         y, por bucket en ese orden, sus claves ordenadas (2.3)
Fields   wCount u8 | wDup u8 | wFirst u8 | wX u8 | wOrigin u8 | origen base u8 | 0 u16 | ID base u32, y por clave, en bits:
           count[wCount] | dup[wDup] | origen−base[wOrigin] | ID−base[wFirst] | V[2+wX] | loc[2]
Heads    reservada para las cabezas de prefijo (3.2): hoy siempre vacía
Exp      expansiones (3.3), una por palabra expandida, en orden de clave
Inline   las listas en línea, pegadas en bits en orden de clave
```

### 3.1 Campos de cada palabra

- `count`: postings de la lista. `dup = count − df`, con `df` los productos distintos (casi siempre 0).
- `V`: con `count = 1`, la posición y la cola del único posting (`pos | cola << 4`); si no, la de 2.1. `wX` es al menos 4, así que siempre hay 6 bits para la posición y la cola.
- `loc`:
  - 0 = un solo posting: está todo en el campo y no hay lista;
  - 1 = lista **en línea**, en la sección `Inline`;
  - 2 = lista en la zona de palabras del `.db`;
  - 3 = lista en el `.db` y además **expandida** (tiene registro en `Exp`).
- **La posición de cada lista no se guarda:** es la suma de los largos de las anteriores con el mismo lugar (las de `Inline` desde el bit 0 de la sección; las del `.db` desde el byte 32 del `.db`).
- Un prefijo es un tramo contiguo de claves, así que sus listas del `.db` son un tramo contiguo de la zona de palabras: se leen con un rango, o con pocos si hay listas expandidas en medio.

### 3.2 Cabezas de prefijo (diferido)

El diseño preveía una cabeza por cada nodo de letras grande (por ejemplo, el prefijo `cam` sobre `camisa`, `camiseta`, `camion`…). **No está implementado**: la sección `Heads` existe con largo 0, para agregarlo sin cambiar el formato. Hoy un prefijo:
- lee entera cada lista chica del `.db` del tramo, dentro de `PrefixBudget` (6.3);
- usa la cabeza propia de cada palabra expandida del tramo (3.3), sin leer su lista.

### 3.3 Expansión de una palabra

Cada registro de la sección `Exp`:

```
word u32 | size u32                     índice de la clave en el bucket; bytes de lo que sigue
groups u8 | p u8 | wLen u8 | 0 u8 | n u32
groups × grupo de cabeza (3.4)
n claves (2.3), cortadas a p unidades; alineado a byte
n × largo[wLen] bits                    bytes de cada bloque de combinaciones en el .db
```

- **Grupos de cabeza:** el primero es el de todas las colas (`tail = 255`). Si la clave tiene dos colas o más (`chocolat` de `chocolate` y `chocolatera`), hay además un grupo por cola. Así la cabeza da el top exacto para cualquier cola de la consulta: los productos con la cola de la consulta (palabra exacta) salen del grupo de esa cola, y los de la misma raíz, del grupo de todas.
- **Directorio de combinaciones:** las n claves son los prefijos de p unidades de las otras palabras de los productos que tienen esta (co‑claves). Cada una nombra un bloque del `.db` (4) con las combinaciones de todas las co‑claves que empiezan así. Las posiciones de los bloques salen de sumar los largos desde `DBCombos`.
- **Elección de p:** 8 (un bloque por co‑clave) salvo que el directorio pase de `ComboDictBudget` (2 KB); en ese caso, el p más grande que entra, hasta 2. Es el "baja otra letra" del diseño.
- **Dónde vive el par {a, k}:** bajo la palabra expandida; si las dos lo están, bajo la clave menor. El lector lo sabe después de la ronda 1, porque leyó los dos `.idx`.

### 3.4 Grupo de cabeza

```
tail u8 | flags u8 (bit 0 = hay colas) | wID u8 | origen base u8 | m u16 | wOrigin u8 | 0 u8 | total u32 | ID base u32 | size u32     (20 B)
m entradas, en bits (size bytes): origen−base[wOrigin] | ID−base[wID] | postings−1 (nibbles) | pos[4] por posting | cola[2] por posting (con el flag)
```

- **Orden:** por `min(primera posición, 2)` (primera palabra, segunda, resto: el bonus de posición de `PLAN_5BIT.md` 13.3) y después por clave ascendente. Es el orden del puntaje de una consulta de ese término solo, así que las m primeras entradas son el top exacto de la palabra.
- **Cada entrada trae todos los postings de la palabra en el producto**, no solo la primera posición: el lector calcula el puntaje exacto.
- `m ≤ H = 64`; `total` son los productos del grupo, para saber si la cabeza los tiene a todos.

---

## 4. `<xy>.db`

```
Header (32 B): magic "GXD1" | Format uint16 = 3 | Group uint16 | NormID uint32 | Epoch uint32 | Seq uint64
               | CRC32C del cuerpo uint32 | Grouping uint8 | reservado 3 B
Zona de palabras        las listas con loc 2 o 3, pegadas en bits en orden de clave
Zona de combinaciones   desde DBCombos: los bloques, en orden de palabra expandida y de entrada del directorio
```

Bloque de combinaciones (se lee entero):

```
n u16 | origen de la base u8 | 0 u8 | ID de la base u32
n co‑claves (2.3), alineadas a byte
por co‑clave, en su orden: las columnas (2.2) de la palabra expandida y de la co‑clave
```

El lector **no lee el header del `.db`**, solo rangos. La consistencia la da el ETag (7). El header y su CRC los verifican los tests y el CLI (`gxs inspect`).

---

## 5. Qué tan específico es el índice: T

Todo se decide por bucket en cada compactación, con D = bytes de todas las listas de palabras del bucket:

> **T = máx(MinSplit, √(ExpandCost × D))**, con MinSplit = ExpandCost = 2 KB.

- **Por qué la raíz:** expandir una palabra agrega ~ExpandCost al `.idx`, y eso lo paga toda consulta que lee ese bucket. El ahorro solo lo reciben las consultas que usan esa palabra, que son más cuanto más frecuente es. El equilibrio entre ambos da la raíz. Como mucho se expanden D/T palabras, así que el `.idx` crece ≈ T por las expansiones y la lectura máxima de una lista en el `.db` es ≈ T: índice y datos crecen juntos.
- **Una palabra se expande** si su lista pasa de T. **Histéresis:** una ya expandida se colapsa recién al bajar de T/2. El conjunto de expandidas se guarda en `tokens`.
- **Listas en línea:** las de palabras no expandidas, de la más chica a la más grande, mientras sumen hasta `InlineBudget` (2 KB). Las de un solo posting no cuentan: van en el campo.
- **Diferido:** las cabezas de nodos de letras (3.2) y la expansión de una combinación a terceras palabras. La profundidad es 2: palabra y par.

Medido en `productos.csv`: palabras expandidas en total, 1 (10k, IDs densos), 8 (10k, aleatorios), 38 (100k, densos) y 77 (100k, aleatorios). El `.idx` más grande es siempre `ca`: 4,0 / 4,5 / 5,4 / 5,8 KB.

---

## 6. Lectura

### 6.1 Rondas

1. **Ronda 1, en paralelo:**
   - `p.log`;
   - el `.idx` de cada bucket que necesitan los términos y que no está en caché: el de la palabra, y los de su rango de prefijo (un número con prefijo puede abarcar varios). Los typos usan el mismo bucket, porque **no hay typos en las dos primeras letras**;
   - si todos los `.idx` estaban en caché, también los rangos del `.db` que ellos indican (10).
2. **Validar** cada `.idx` contra la base del log (7). Si algo no vale, la búsqueda vuelve a empezar.
3. **Elegir las palabras de cada término** (picks):
   - la exacta;
   - si es la última palabra (o termina en `*`), las del prefijo, dentro de `PrefixBudget`;
   - con `Fuzzy` y solo si el término no tiene ni exacta ni prefijo, las del mismo bucket a edit distance 1–2 (`PLAN_5BIT.md` 7.4), también dentro del presupuesto.
4. **Plan de lectura:**
   - **Con cabeza:** una palabra expandida que sirve a un solo término, como exacta o como prefijo. No se lee su lista: está la cabeza en el `.idx`.
   - **Completa:** todas las demás. Si la lista está en el `.db`, se lee el rango.
   - **Combinaciones:** por cada par de picks en que al menos uno va con cabeza, el bloque del par bajo su ancla (3.3).
5. **Ronda 2:** los rangos del `.db`, fusionados cuando están a menos de `CoalesceGap` (4 KB), en paralelo. Si todo estaba en el `.idx`, no hay ronda 2.
6. **Armado,** en este orden: listas completas, combinaciones, cabezas y el overlay de `p.log`. Un producto con op pendiente se saca y se vuelve a evaluar con sus claves nuevas.
7. **Verificación de exactitud:** se ordena con el ranking de siempre. Para cada pick con cabeza incompleta se calcula el mejor puntaje que podría tener un producto que no está en la cabeza. Si el resultado número `offset + limit` no lo supera, se lee la lista completa de esa palabra (**ronda 3**) y se arma de nuevo. Pasa en páginas profundas o cuando casi toda la cabeza tiene también otros términos. Con los parámetros por defecto no pasó en ninguna de las consultas medidas (13).

Por qué es exacto: todo producto con dos picks o más se ve entero, por una lista completa o por una combinación. Un producto que solo tiene una palabra con cabeza está en la cabeza o no puede superar su última entrada.

### 6.2 IDF

El `df` de un término es la **suma de los `DF` de las claves del diccionario que le corresponden** (exacta, prefijo, typos), y N es `Docs` de la base del log. Un producto con dos claves del mismo término cuenta dos veces. No hace falta leer ninguna lista para el IDF, y la referencia de fuerza bruta de los tests usa la misma regla.

Antes el `df` contaba los candidatos después del overlay. Ahora es el del índice: las ops pendientes son pocas (`CompactBytes`). La colección sin índice (solo `p.log`) sigue contando sus documentos vivos.

### 6.3 Presupuesto de prefijos

Un prefijo o un typo que tiene que leer muchas listas sigue limitado por `PrefixBudget` (32 KB, primero las más frecuentes; para typos, primero las de menor distancia). Solo cuestan las listas del `.db` de palabras no expandidas: las de campo y en línea ya están en el `.idx`, y las expandidas se leen por su cabeza. Si queda alguna clave afuera se marca `Approx`, como en `PLAN_5BIT.md` 7.3. Un término sin exacta toma siempre su mejor clave, aunque pase el presupuesto.

---

## 7. Consistencia sin generaciones

**Base de `p.log` (Format 3, 176 B):**

```
0   magic "GXL1"   4
4   Format         uint16 = 3
6   flags          uint16       bit 0 = LogOnly; byte alto (7) = Grouping del índice (14)
8   NormID         uint32
12  Epoch          uint32
16  LastSeq        uint64       las ops ≤ LastSeq están en el índice (o en Docs, sin índice)
24  PrevSeq        uint64       el LastSeq de la base anterior (caché en caliente, 10)
32  UnixMs         int64        cuándo se escribió
40  Docs           uint32       documentos a LastSeq
44  Touched        [128]byte    bit s = la compactación que escribió esta base reescribió el objeto s (bucket, o grupo con Grouping)
172 CRC32C         uint32
```

Los frames (`Batch`, `Compact`, `Docs`) son los de `PLAN_5BIT.md`, salvo que cada documento se nombra con su clave de 5 bytes (`ID u32 | origen u8`, 15).

**Reglas del lector**, para cada `.idx` de la ronda 1 (el header alcanza para decidir):

| El `.idx` | Con `Touched[s]` | Sin `Touched[s]` |
|---|---|---|
| no existe (404) | viejo: S3 todavía no lo mostraba | vacío |
| `Epoch` mayor que la base | viejo (una reescritura completa todavía no recortó el log) | viejo |
| `Epoch` menor que la base | viejo | vacío (basura de un índice reemplazado, 8) |
| misma `Epoch`, `Seq < LastSeq` | viejo | vigente |
| misma `Epoch`, `Seq ≥ LastSeq` | vigente | vigente |

- Un `.idx` más nuevo que el log (la compactación lo escribió y todavía no recortó el log) es inofensivo: el log se aplica entero, y una op ya incorporada da el mismo producto.
- **Cada rango del `.db`** trae el ETag del objeto. Si no es el `DBETag` del `.idx`, o el `.db` da 404, el bucket se reescribió entre rondas.
- **Una ronda 1 que leyó algún `.idx` y tardó más de `RoundLimit` (500 ms)** se descarta. Junto con `CompactSpacing` (1 s entre compactaciones), impide ver un `.idx` de dos compactaciones atrás con el log actual, que es lo único que las reglas de arriba no detectan.
- **"Viejo" vuelve a empezar la búsqueda**, con backoff aleatorio: hasta 8 intentos y unos 3 s en total. Es lo que tiene una reescritura completa para terminar de escribir sus buckets.

**Época:** sube en cada reescritura completa (8). Un `.idx` de otra época nunca se mezcla.

---

## 8. Escritura

**Append:** igual que `PLAN_5BIT.md` 8: `Apply` es una lectura y un append, con `CompactBytes` y `MinIndexDocs`.

**Lease** (`compact.lease`, 24 B: `"GXC1" | owner u64 | vencimiento ms i64 | época u32`):
- Se crea con `If-None-Match`. Si existe y está vencido (`LeaseTimeout`, 2 min), se toma con `If-Match`: es una **recuperación**, y la compactación reescribe todo (ver abajo). Si no está vencido: `ErrConflict`.
- Antes de escribir buckets en una época nueva, la anota en el lease (`If-Match`). La época nueva es `máx(base, tokens, lease) + 1`: nunca se reusa una que un dueño anterior pudo haber escrito a medias.
- Al terminar se borra, si sigue siendo nuestro. Si la compactación falla **después** de escribir algún bucket, se deja vencido: la siguiente lo toma de inmediato y reescribe todo.

**Compactación** (`Compact`):

1. Toma el lease. Lee `p.log` y `tokens` en paralelo, y aplica las ops pendientes al estado `documento → claves`. Si no hay nada que hacer (sin ops, sin cambio de modo, sin restos), termina sin escribir.
2. Espera hasta `base.UnixMs + CompactSpacing`, salvo que la base sea `LogOnly` y no haya `tokens`: entonces nunca hubo buckets que confundir, y la primera indexación (`Init`, el paso a tener índice) no espera.
3. **Bajo `MinIndexDocs`:** recorta `p.log` a una base `LogOnly` con `Docs`. Si había índice, la base sube de época y el índice viejo se borra en la siguiente compactación, cuando ninguna consulta lo lee.
4. **Reescritura completa** (época nueva) si: hubo recuperación, la base era `LogOnly`, falta `tokens`, o `tokens` es de otra época. Si no, **incremental**.
5. **Construye en memoria el índice entero** a partir del estado: listas, D y T por bucket, y expansiones con su histéresis. Cada bucket sale con un hash de contenido (FNV‑64a de sus secciones y su `.db`).
6. **Escribe los buckets cuyo hash cambió** respecto del que guarda `tokens` (todos, si es completa), en paralelo. Primero `<xy>.db` y se toma su ETag; después `<xy>.idx` con ese ETag y `Seq = TargetSeq`. Comparar el contenido cubre todo lo que cambia un bucket: sus palabras, las expansiones que cambian por histéresis, y los pares que se mudan cuando otra palabra se expande o se colapsa.
7. **Un bucket que quedó vacío** deja una lápida (`Keys = 0`) y su `.db` se borra. La lápida se anota como basura.
8. **Borra la basura de la compactación anterior:** las lápidas, y en una reescritura completa los buckets del índice viejo que el nuevo no tiene. Ninguna consulta los lee ya: para la base actual son vacíos (7), y pasó al menos `CompactSpacing`.
9. Escribe `tokens`: el estado, las expandidas, el hash de cada bucket y si tiene `.db`, y la basura nueva.
10. **Recorta `p.log`** con `If-Match`. Es el commit. La base nueva lleva `LastSeq = TargetSeq`, `PrevSeq`, `Docs` y `Touched` (los buckets escritos y las lápidas), y conserva las ops y los pedidos posteriores.

**Una caída a mitad de camino:**
- `.idx` más nuevos que el log valen (7).
- La siguiente compactación encuentra el lease (vencido) y reescribe todo en una época nueva. Así ningún bucket que quedó escrito a medias se compara contra un hash de `tokens` que ya no le corresponde.
- **Costo de una caída en una reescritura completa:** los buckets que ya se escribieron tienen una época mayor que la base, y las consultas que los necesitan fallan hasta que otra compactación termine (12.3).

**Costo:** hasta 2 PUT por bucket que cambió, más `tokens` y el log. Lo que se reescribe es proporcional a los buckets que cambiaron, pero cada `.db` se reescribe entero, y los buckets grandes (`ca`, `co`, `po`) cambian casi en cada compactación (13.4).

---

## 9. Reconstrucción, vaciado y colecciones chicas

- **`Rebuild`:**
  1. lease (si está tomado, espera y reintenta);
  2. anota el último `seq` del log, S0, **antes** de leer la fuente (si no hay `p.log`, lo crea vacío);
  3. lee la fuente y hace una reescritura completa (8) con ese estado;
  4. recorta el log conservando las ops posteriores a S0.

  El razonamiento de `PLAN_5BIT.md` 8 sobre S0 no cambia. Con otro normalizador (otras letras extra), el log viejo se reemplaza.
- **`Drop`:** espera `CompactSpacing`, deja un `p.log` `LogOnly` vacío en una época nueva y borra `tokens`, `.idx` y `.db`.
- **`Init`:** escribe un `p.log` `LogOnly` con los documentos (`If-None-Match`; `ErrExists` si ya hay uno) y, si pasan de `MinIndexDocs`, compacta. Esa primera compactación es una reescritura completa.
- **Colección chica:** sin cambios. Al pasar a tener índice o al dejar de tenerlo, sube la época.

---

## 10. Caché en caliente

- **Qué se guarda:** el motor guarda los `.idx` ya leídos (decodificados) y la `(Epoch, LastSeq)` en que valen, hasta `CacheBytes` (64 MB).
- **Cuándo sigue valiendo**, al leer la base:
  - misma `LastSeq`: todo;
  - `PrevSeq` igual a la de la caché: todo menos los buckets de `Touched`;
  - una base más vieja que la caché (otra réplica de S3 atrasada): no se usa ni se llena;
  - cualquier otro caso: se vacía.
- **Consulta en caliente:** los rangos del `.db` se piden en la ronda 1, junto con `p.log`, con el plan del `.idx` en caché. Se validan después, con la base y el ETag. Si un `.idx` dejó de valer, se relee y el plan se rehace (una ronda más).
- **Resultado:** 1 ronda; 1 GET si todo está en el `.idx`.

---

## 11. Parámetros

| Parámetro | Valor | Dónde |
|---|---|---|
| `MinSplit` | 2 KB | mínimo de T (5) |
| `ExpandCost` | 2 KB | costo de una expansión en el `.idx` (5) |
| `HeadSize` (H) | 64 | entradas de una cabeza: 3 páginas de 20 más margen |
| `InlineBudget` | 2 KB | listas en línea por `.idx` |
| `ComboDictBudget` | 2 KB | directorio de combinaciones antes de agrupar por prefijo |
| Profundidad | 2 palabras | 3 queda diferido |
| `CompactSpacing` | 1 s | 7, 8 |
| `LeaseTimeout` | 2 min | lease vencido |
| `CompactBytes` | 4 KB | sin cambios |
| `MinIndexDocs` | 100 | sin cambios |
| `PrefixBudget` | 32 KB | 6.3 |
| `CoalesceGap` | 4 KB | fusión de rangos del `.db` |
| `MaxRequest` | 1 MB | rango más largo de un request |
| `RoundLimit` | 500 ms | ronda 1 descartada por lenta (7) |
| `CacheBytes` | 64 MB | caché de `.idx` del motor |
| `Concurrency` | 16 lectura / 32 escritura | requests en paralelo |

Los de construcción son configurables (`collection.Options.Params`). Cambiarlos solo afecta a lo que se escribe desde ese momento: el lector no los necesita, porque el `.idx` dice qué está expandido y dónde está cada cosa.

---

## 12. Implementación

### 12.1 Paquetes

| Paquete | Qué hace |
|---|---|
| `blob` | `GetRange` devuelve también el ETag del objeto (S3 lo manda en cada rango) |
| `plog` | base de 176 B con `Epoch`, `PrevSeq`, `Docs` y `Touched` (7) |
| `postings` | listas de una palabra (2.1), columnas (2.2) y entradas de cabeza (3.4) |
| `key5` | streams de claves de cualquier bucket (2.3) |
| `layout` | nombres de los objetos de una partición y agrupaciones de buckets (14) |
| `bucket` | formatos `.idx` / `.db` y la construcción de todos los buckets a partir del estado |
| `tokens` | el objeto `tokens`: estado por chunks de 1 000 documentos con CRC, expandidas, hash por bucket y basura |
| `collection` | lease, compactación incremental, reescritura completa, `Rebuild`, `Drop`, `Init` |
| `query` | plan, rondas, validación, caché, armado, verificación de la cabeza, ranking |
| `cmd/gxs` | `build`, `query`, `put`, `del`, `compact`, `log`, `inspect` |

Se borraron `index`, `manifest` y `snapshot`. La API pública (`Client`, `Apply`, `Search`, `Compact`, `Rebuild`, `Drop`) no cambió, salvo `Response.Gen`, que pasó a ser `Response.Seq`. El backend `genixs3` de Genix ORM compila y pasa sus tests sin cambios.

### 12.2 Decisiones tomadas en la implementación

- **Qué buckets se reescriben:** los de hash de contenido distinto (8.6), en vez de calcular los tocados a partir de las claves viejas y nuevas. Es exacto y más simple, y cubre los pares que se mudan de bucket cuando cambia una expansión.
- **La recuperación de un lease vencido reescribe todo en época nueva.** La alternativa (recalcular qué quedó a medias) necesitaba saber qué escribió el dueño anterior.
- **Basura y lápidas se borran en la compactación siguiente**, no en la misma: así ninguna consulta en curso pierde un objeto que acaba de validar.
- **Reglas de época del lector** (la tabla de 7): un `.idx` de época menor sin `Touched` es basura y cuenta como vacío, y uno de época mayor siempre es viejo.
- **`Init` es un log `LogOnly` más una compactación**, no una reconstrucción.
- **IDF por suma de `DF`** (6.2).
- **Las cabezas guardan todos los postings** de la palabra en cada producto (3.4), no una máscara de colas.
- **El directorio de combinaciones** no tiene entradas exactas mezcladas con grupos, ni `df`: o todo exacto (p = 8) o todo agrupado por p unidades (3.3).
- **Typos solo en el mismo bucket** y solo con `Request.Fuzzy`.
- **Reintentos de una búsqueda:** 8, con backoff aleatorio de base 25 ms (unos 3 s como máximo).
- **La primera indexación no espera `CompactSpacing`** (8.2).

### 12.3 Pendiente

- **Cabezas de prefijo** (3.2) y **combinaciones de 3 palabras**. El formato tiene lugar para las primeras (`HeadsLen`). Las mediciones de 13 no muestran que hagan falta todavía: ninguna consulta leyó listas enteras para completar una cabeza.
- **Una reescritura completa que falla a mitad** deja buckets de una época mayor que la base. Las consultas que los necesitan fallan ("the index kept changing") hasta que corre otra compactación, que hace la recuperación. Si después no hay escrituras, nadie la dispara. Opciones: que el `Client` reintente la compactación después de un fallo, o que `Search` devuelva un error tipado para que el que llama la pida.
- **El lease no se renueva.** Una compactación o un `Rebuild` que pase de `LeaseTimeout` (2 min, por ejemplo por una fuente lenta) puede perderlo. Lo detecta al anotar la época (antes de escribir) o al recortar el log, pero en el segundo caso ya escribió buckets.
- **Costo de escritura:** a 100k, una compactación de 72 ops escribe 11,3 MB (13.4), porque los `.db` grandes cambian en casi toda compactación y se reescriben enteros.

---

## 13. Medición

`productos.csv` (10 010 nombres) y ×10 (100 100), con IDs densos (1..N) o `uint32` aleatorios. Consultas: 3 000 de 2–3 palabras consecutivas de nombres del catálogo, y 1 500 de 1 palabra (la primera de esas consultas). La última palabra también es prefijo; top 10. Store local con contador de requests y bytes (los bytes incluyen `p.log`). "En caliente" es la misma consulta por segunda vez en el mismo motor; "en frío", un motor nuevo por consulta.

`GXS_EVAL_CSV=../../../scripts/perf/data/productos.csv go test ./internal/query -run Eval -v`

### 13.1 Tamaño

| | `.idx` (379) total · prom. · máx. | `.db` cuántos · total · máx. | Expandidas | `tokens` | Build |
|---|---|---|---:|---:|---|
| 10k, IDs densos | 157 KB · 423 B · 4,0 KB | 3 · 55 KB · 53 KB | 1 | 338 KB | 52 ms · 388 PUT · 0,86 MB |
| 10k, aleatorios | 271 KB · 733 B · 4,5 KB | 42 · 470 KB · 103 KB | 8 | 338 KB | 61 ms · 427 PUT · 1,38 MB |
| 100k, densos | 426 KB · 1,1 KB · 5,4 KB | 126 · 3,90 MB · 381 KB | 38 | 3,27 MB | 450 ms · 511 PUT · 10,8 MB |
| 100k, aleatorios | 556 KB · 1,5 KB · 5,8 KB | 172 · 9,89 MB · 757 KB | 77 | 3,27 MB | 587 ms · 557 PUT · 17,0 MB |

El `.idx` más grande es `ca` en los cuatro casos. Pasar de 10k a 100k multiplica el `.idx` máximo por 1,3: es la raíz de 5.

### 13.2 Lecturas por consulta

`p.log` recién compactado pesa 176 B (la base). En caliente, un p50 de 176 B quiere decir que la mitad de las consultas solo leyó `p.log`.

**IDs `uint32` aleatorios** (el caso general):

| | Palabras | Rondas | Requests | Bytes prom. / p50 / p99 |
|---|---:|---:|---:|---:|
| 10k, en caliente | 1 | 1 | 1,26 | 463 B / 176 B / 4,2 KB |
| | 2 | 1 | 1,63 | 858 B / 176 B / 4,5 KB |
| | 3 | 1 | 2,13 | 1,5 KB / 678 B / 10,0 KB |
| 10k, en frío | 1 | 1,26 | 2,39 | 2,7 KB / 2,6 KB / 8,7 KB |
| | 2 | 1,50 | 3,90 | 5,0 KB / 4,7 KB / 11,7 KB |
| | 3 | 1,64 | 5,42 | 7,7 KB / 7,4 KB / 17,9 KB |
| 100k, en caliente | 1 | 1 | 1,77 | 1,5 KB / 773 B / 10,0 KB |
| | 2 | 1 | 2,79 | 4,4 KB / 2,7 KB / 29,6 KB |
| | 3 | 1 | 4,35 | 8,4 KB / 5,5 KB / 39,4 KB |
| 100k, en frío | 1 | 1,71 | 2,90 | 4,8 KB / 4,5 KB / 14,0 KB |
| | 2 | 1,97 | 5,06 | 10,8 KB / 9,3 KB / 36,0 KB |
| | 3 | 1,99 | 7,64 | 17,8 KB / 15,3 KB / 50,3 KB |

**IDs densos** (1..N):

| | Palabras | Rondas | Requests | Bytes prom. / p50 / p99 |
|---|---:|---:|---:|---:|
| 10k, en caliente | 1 | 1 | 1,04 | 214 B / 176 B / 1,5 KB |
| | 2 | 1 | 1,11 | 266 B / 176 B / 1,5 KB |
| | 3 | 1 | 1,20 | 368 B / 176 B / 3,2 KB |
| 10k, en frío | 1 | 1,04 | 2,17 | 1,7 KB / 1,4 KB / 5,5 KB |
| | 2 | 1,11 | 3,38 | 3,0 KB / 2,6 KB / 7,2 KB |
| | 3 | 1,15 | 4,49 | 4,5 KB / 4,2 KB / 10,0 KB |
| 100k, en caliente | 1 | 1 | 1,65 | 904 B / 405 B / 6,3 KB |
| | 2 | 1 | 2,53 | 2,4 KB / 1,4 KB / 13,1 KB |
| | 3 | 1 | 3,81 | 4,5 KB / 2,8 KB / 20,7 KB |
| 100k, en frío | 1 | 1,63 | 2,78 | 3,6 KB / 3,4 KB / 9,2 KB |
| | 2 | 1,92 | 4,79 | 7,7 KB / 7,1 KB / 19,0 KB |
| | 3 | 1,97 | 7,10 | 12,3 KB / 11,1 KB / 28,5 KB |

**Contra el diseño anterior** (`PLAN_5BIT.md` 13.2, las mismas 3 000 consultas de 2–3 palabras):

| | Antes: rondas · requests · bytes prom. | Ahora |
|---|---|---|
| 10k densos, en caliente | 1 · 3,30 · 1,0 KB | 1 · **1,15** · **316 B** |
| 10k aleatorios, en caliente | 1 · 3,38 · 2,4 KB | 1 · **1,88** · **1,2 KB** |
| 100k densos, en caliente | 1 · 3,56 · 7,2 KB | 1 · 3,16 · **3,4 KB** |
| 100k aleatorios, en caliente | 1 · 3,57 · 20,4 KB | 1 · 3,56 · **6,4 KB** |
| 100k densos, en frío | 4 · 7,76 · 17,4 KB | **1,94** · 5,94 · **10,0 KB** |
| 100k aleatorios, en frío | 4 · 7,80 · 30,8 KB | **1,98** · 6,34 · **14,3 KB** |

Qué dicen:
- **En frío:** 2 rondas como máximo, en vez de 4. Los bytes bajan a la mitad.
- **En caliente, a 10k:** casi todo está en el `.idx`. 1,15–1,88 requests, contra ~3,4.
- **En caliente, a 100k:** los bytes bajan ×2,1 (densos) y ×3,2 (aleatorios). Los requests no bajan con IDs aleatorios: siguen siendo `p.log` y un rango por cada `.db` que se toca (listas y bloques de combinaciones). Las palabras de una consulta casi siempre están en buckets distintos, así que sus rangos no se pueden fusionar: `CoalesceGap` cambia menos de 0,1 request.
- **Las cabezas alcanzan siempre:** ninguna consulta leyó una lista entera para completar una cabeza (ronda 3) con los parámetros por defecto. A 100k con IDs aleatorios, 932 de 1 488 consultas de 3 palabras usaron alguna cabeza, y leyeron 5,1 combinaciones en promedio.
- **`Approx` nunca se marcó:** ningún prefijo quedó cortado por `PrefixBudget`.
- **`p.log` lleno** (4,2 KB, 72 appends de un documento), a 100k con IDs aleatorios: 10,4 KB en caliente, contra 24,1 KB antes. A 10k: 5,2 KB.

### 13.3 Ranking

| Comparación (IDs aleatorios) | 10k | 100k |
|---|---|---|
| Motor contra las mismas reglas aplicadas a todos los documentos (fuerza bruta) | **idéntico en 3 000/3 000** | **idéntico en 3 000/3 000** |
| Contra un ranking por palabras completas (sin corte en 8): orden idéntico · mismos IDs · solapamiento | 89,5 % · 90,2 % · 95,2 % | 99,2 % · 99,2 % · 99,5 % |

Los números contra palabras completas son los mismos de `PLAN_5BIT.md` 13.3: el ranking no cambió con el formato.

En los tests (`TestMatchesBruteForce`, 3 000 documentos con IDs aleatorios), el top coincide con la fuerza bruta también con parámetros chicos para que casi todo se expanda: cabezas de 8 y de 1 entrada, combinaciones agrupadas por prefijo, páginas profundas, ops pendientes en `p.log` y typos. Ahí la ronda 3 sí se ejercita: 96 y 112 de 406 consultas leyeron listas enteras.

**Typos** (500 consultas de 2 palabras de un mismo producto, una con 1 error: transposición, borrado, inserción o sustitución):

| | 10k | 100k |
|---|---|---|
| Con el error en las dos primeras letras (no se buscan, por diseño) | 181 | 188 |
| El primer resultado cubre las dos palabras | 307/500 (96 % de las 319 con el error después de la 2.ª letra) | 303/500 (97 % de 312) |
| El producto queda en el top 10 | 407/500 | 338/500 |
| Lo mismo sin `PrefixBudget` | 407/500 | 338/500 |

- **El presupuesto no corta ningún typo:** sin él da lo mismo.
- **Lo que se pierde es el error en las dos primeras letras**, por diseño. El diseño anterior lo buscaba en los slots vecinos: 482/500 y 457/500 a 10k.
- A 100k el producto queda en el top 10 menos veces, aunque la cobertura es la misma. La explicación probable: con el error en las dos primeras letras solo cuenta la otra palabra, y a 100k esa palabra empata con 10 veces más productos.

### 13.4 Escritura

| IDs aleatorios | 10k | 100k |
|---|---|---|
| Build inicial | 61 ms · 427 PUT · 1,38 MB | 587 ms · 557 PUT · 17,0 MB |
| `Put` de 1 documento | 1 GET (176 B) + 1 append (52 B) | 1 GET (176 B) + 1 append (65 B) |
| `p.log` hasta el umbral de compactación | 71 appends de un documento | 72 appends |
| Compactación | 71 ops · 37 ms · 14 buckets · 4 lecturas (347 KB) · 28 escrituras (782 KB) | 72 ops · 410 ms · 62 buckets · 4 lecturas (3,27 MB) · 128 escrituras (11,3 MB) |

**Es más cara que antes**, y `PLAN_5BIT.md` pide que la escritura siga siendo eficiente: queda para decidir (12.3). La escritura de un documento no cambió (1 GET y 1 append); lo que cambió es la compactación. El diseño anterior compactaba 100k en 5 escrituras y 6,0 MB, porque reescribía `k`, `p` y el snapshot enteros. Ahora son 128 escrituras y 11,3 MB:
- 72 productos al azar cambian 62 de los 379 buckets, casi todos con `.db`;
- entre ellos están casi siempre los buckets grandes (`ca`, `co`, `po`…), y cada `.db` se reescribe entero (el más grande pesa 757 KB): se escriben unos 7,9 de los 9,9 MB de `.db`;
- `tokens` (3,3 MB) se reescribe en cada compactación, como antes el snapshot.

Lo que más lo bajaría es que el `.db` sea append‑only, aprovechando el append nativo de S3 Express: la compactación agrega al final solo las listas que cambiaron, y el `.idx` apunta a los offsets nuevos. Habría que validar los rangos por tamaño en vez de por ETag (un append cambia el ETag), y reescribir el `.db` cuando la basura pase de la mitad. No está implementado.

### 13.5 S3 Express real

`GENIX_S3_TEST_BUCKET=genix-test--use1-az4--x-s3 go test -race ./s3blob`. Las pruebas corrieron desde fuera de AWS, con unos 100 ms por request. Desde la misma AZ serían unos pocos ms.

- **Contrato** (`blobtest`): pasa, ahora también con el ETag en cada rango.
- **Ciclo de vida** (`TestLifecycle`, 300 documentos):
  - "gorra adidas" en frío: 1 ronda, 3 requests, 421 B. En caliente: 1 request, 176 B.
  - 40 `Put` desde 4 escritores concurrentes y un `Delete`.
  - La compactación de 41 ops reescribió 10 buckets: 4 lecturas, 14 escrituras, 10,7 KB.
  - Después de compactar, "mochila nike negro" en frío: 1 ronda, 4 requests, 542 B.
- **`Client` multi‑partición** (`TestClient`):
  - Una partición nueva se crea en 2 requests y se consulta en 1 ronda (`LogOnly`).
  - Reconstruir 400 documentos con un alta y una baja concurrentes escribió 21 buckets: 27 escrituras, 14 KB. Las dos escrituras concurrentes se conservaron.
  - `Drop` deja la partición vacía.

---

## 14. Agrupación de buckets (flag `Grouping`)

> **Activada por defecto en genix-s3 desde el 2026-10-03** (el valor cero del flag es `GroupingES100`). Con `GroupingNone` un objeto es un bucket y todo lo anterior queda igual.

**Qué cambia:** con `GroupingES100`, los 1 024 buckets se reparten en **100 grupos de buckets contiguos** (en el orden de las unidades de 5 bits), y cada grupo es un par `.idx` / `.db`. Así hay como mucho 200 objetos, en vez de 2 048. A 100k con IDs aleatorios son 185, en vez de 551.

**Cómo se activa:**
- `genix.Config.Grouping` y `genix.ClientOptions.Grouping`: `GroupingES100` (el valor cero, por defecto) o `GroupingNone`;
- `gxs -grouping es100|none` (por defecto `es100`).

Los tests internos y el eval (`GXS_EVAL_GROUPING`) siguen usando `none` salvo que se pida otra, para que las mediciones de 13 sigan siendo comparables.

Solo lo usan los escritores. Un lector sigue al índice, sea cual sea su flag.

### 14.1 La tabla

- **Es fija, está en el código** (`internal/layout/grouping.go`) y no sale del corpus. El nombre del objeto sigue saliendo de la consulta (bucket → grupo), así que la ronda 1 sigue sin directorio.
- **Cada grupo es una corrida de buckets:**
  - un grupo de un solo bucket se llama como el bucket: `ca`, `co`, `de`, `pa`, `pr`…;
  - uno de varios se llama `<primero>-<último>`: `vo-y0` (el resto de la `v`, la `w`, la `x` y `y0`), `ya-z5` (la `y` y la `z`).
- **Cómo se cortó** (el test `TestES100` la reconstruye y la compara):
  - **El peso de cada bucket de letras** es la mitad de su parte de las claves distintas y la mitad de su parte de las ocurrencias. Sale de las 50 000 palabras más frecuentes del español de OpenSubtitles 2018 (`hermitdave/FrequencyWords`, `content/2018/es/es_50k.txt`).
  - **Los números** no aparecen en esas listas. Para ellos, un valor n pesa 1/(n+1), y en conjunto tienen el 20 % del peso. Sin ese peso, todos los números quedaban en un solo grupo de 102 KB.
  - **El corte:** se busca, por bisección, el peso máximo por grupo más chico que da 100 corridas o menos. Un bucket más pesado que ese máximo queda solo en su grupo.
  - Quedan 79 grupos de letras y 21 de números.

### 14.2 Formatos

| Objeto | Dónde se anota la agrupación |
|---|---|
| Base de `p.log` | byte alto de los flags (7): la agrupación del índice |
| `.idx` | `Group` (24) y `Grouping` (26) |
| `.db` | `Group` (6) y `Grouping` (28) |
| `tokens` | `Grouping` (offset 40); la basura lleva su agrupación |

- **La sección `Keys` de un `.idx`** tiene un directorio de los buckets del grupo con su cantidad de claves, y después el stream de claves de cada bucket (3).
- **`Touched` marca grupos.**
- **La construcción decide todo por bucket, igual que antes:** T, las expansiones y `InlineBudget`. Un grupo es la concatenación de lo que tendrían sus buckets por separado. Por eso el `.idx` de un grupo pesa la suma de los `.idx` de sus buckets, y las rondas y los rangos del `.db` no cambian.

### 14.3 Cambiar el flag

- **El escritor manda.** Una compactación con una agrupación distinta de la de la base hace una **reescritura completa en época nueva**, aunque no haya ops.
- **La basura:**
  - Los objetos de la agrupación vieja pasan a ser basura, anotada con su agrupación. La compactación siguiente los borra por nombre.
  - No se borran los que el índice nuevo reescribió con el mismo nombre (`ca.idx` existe en las dos).
  - Un `.db` viejo se borra si el grupo nuevo del mismo nombre no tiene `.db`.
- **El lector sigue a la base.** Si la base de `p.log` dice otra agrupación que la del motor:
  - el motor se cambia a esa agrupación;
  - vacía su caché;
  - y la búsqueda vuelve a empezar sin backoff. Es 1 reintento, una sola vez.
  
  Una base `LogOnly` no tiene buckets, así que ahí la agrupación no importa.
- **Todos los escritores de una partición deben usar el mismo flag.** Si no, cada compactación de un escritor distinto reescribe todo.

### 14.4 Medición

Mismo eval que 13, con `GXS_EVAL_GROUPING=es100`.

**Lo que no cambia:**
- el ranking (idéntico a la fuerza bruta en 3 000/3 000);
- los typos;
- las rondas;
- **todas las consultas en caliente**, byte por byte: no leen el `.idx`.

**Lo que cambia es el `.idx` en frío.**

| IDs aleatorios | none | es100 |
|---|---|---|
| Objetos, 10k | 421 (379 `.idx` + 42 `.db`) | 137 (97 + 40) |
| Objetos, 100k | 551 (379 + 172) | 185 (97 + 88) |
| `.idx` más grande, 100k | 5,8 KB (`ca`) | 15,8 KB (`ju-l0`) |
| `.db` más grande, 100k | 757 KB | 800 KB |
| Build, 100k | 557 PUT · 17,0 MB | 191 PUT · 16,9 MB |
| En frío, 100k, 1 · 2 · 3 palabras: bytes prom. | 4,9 · 10,8 · 17,8 KB | 8,3 · 18,5 · 29,0 KB |
| En frío, 100k: requests | 2,90 · 5,06 · 7,64 | 2,89 · 5,03 · 7,59 |
| En frío, 10k: bytes prom. | 2,7 · 5,0 · 7,8 KB | 4,1 · 8,1 · 12,2 KB |
| Compactación de 72 ops, 100k | 62 buckets · 128 escrituras · 11,3 MB | 49 grupos · 102 escrituras · 12,4 MB |
| Compactación de 71 ops, 10k | 14 · 28 · 782 KB | 14 · 29 · 803 KB |

Con IDs densos pasa lo mismo:
- 100k: 505 → 178 objetos;
- en frío, 3,7 · 7,7 · 12,3 KB → 6,1 · 13,5 · 20,7 KB.

Qué dicen:
- **Un tercio de los objetos.** Hay 3 veces menos objetos y 3 veces menos PUT en el build.
- **En frío se lee unas 1,7 veces más**, porque un `.idx` trae los buckets vecinos.
  - Los requests bajan apenas: los términos de una consulta casi nunca caen en el mismo grupo.
  - El costo sigue al `.idx`: solo `.idx`, a 100k se pasa de 3,3 · 6,3 · 9,5 KB a 6,7 · 14,0 · 20,6 KB.
  - Un grupo ideal, cortado sobre el mismo catálogo, daría 6,0 · 11,9 · 17,6 KB (simulación). Eso dice que la tabla genérica del español queda cerca de la ideal.
- **En caliente no hay costo**, salvo que la caché invalida por grupo. Una compactación que toca un bucket obliga a releer todo su grupo.
- **La compactación** hace un 20 % menos de escrituras, pero un 10 % más de bytes, porque los `.db` de grupo son más grandes. No resuelve el costo de escritura de 13.4.
- **Cuándo conviene:**
  - muchas particiones (multi-tenant), donde la cantidad de objetos pesa;
  - consultas casi siempre en caliente: un proceso de vida larga con su caché.
  
  Si la mayoría de las consultas son en frío y se paga por byte, conviene `GroupingNone`.

---

## 15. Origen del documento

> Agregado el 2026-10-05. Un documento es un **origen** (`uint8`) y un **ID** (`uint32`) único dentro del origen: el mismo ID en dos orígenes son dos documentos. Sirve para tener varias tablas en un índice, para que un agente busque un nombre sin saber qué es. Un documento nunca cambia de origen, la búsqueda no filtra por origen y el origen no entra en el puntaje: cada resultado trae su origen y su ID.
>
> Hasta el 2026-10-05 se llamaba "grupo del documento" (`Group`); se renombró porque "grupo" pasa a ser el grupo de texto del documento. No confundir con los grupos de buckets de 14 ni con los grupos de cabeza de 3.4.

### 15.1 La clave

- Todo el índice trabaja con la **clave del documento**, `origen << 32 | ID` (40 bits, paquete `docid`). Las listas, las cabezas y las combinaciones están ordenadas por clave: por origen y, dentro del origen, por ID.
- **Desempate:** con la misma cobertura y el mismo puntaje, va primero la clave menor (origen menor, después ID menor). Es el mismo orden de las cabezas, que tiene que coincidir con el del ranking para que la cabeza dé el top exacto (6.1).
- **En `p.log` y `tokens`** la clave ocupa 5 bytes: `ID u32 | origen u8`. El directorio de chunks de `tokens` guarda la primera clave en un u64 (20 B por chunk).
- **API:** `Doc{OriginID, ID, Text}`, `Ref{OriginID, ID}` para borrar, `Hit{OriginID, ID, …}`, `QueryHit.OriginID` (`cb:"4"`). En el CLI, un documento es `ID` (origen 0) o `ORIGEN:ID`.
- Formatos: `p.log`, `.idx`, `.db` y `tokens` pasan a `Format = 3`. Los índices de antes se reconstruyen (`Rebuild`).

### 15.2 Cómo se codifica sin pagar el origen en cada documento

Una lista es una corrida por origen. Dentro de una corrida, la diferencia de claves es la de los IDs. **El salto a la corrida siguiente es enorme** (≈ Δorigen · 2³²): con los formatos de antes, ensanchaba `W` a 33 bits o más para todas las diferencias de la lista. Por eso:

- **Listas (2.1) y combinaciones (2.2):** hay tres formas, y se elige la más chica:
  - **Codec 0 simple:** `W` es el de la mayor diferencia. Es el de siempre.
  - **Codec 0 con escapes:** `W` es el de la mayor diferencia dentro de una corrida, más uno. El valor de `W` bits todo en unos es un **escape**: le siguen los orígenes salteados (`Δorigen − 1`) y el ID en el origen nuevo, los dos en varint de nibbles. Para que el largo de la lista se pueda calcular desde el diccionario, `X = W | nibbles de escape << 6`. En las combinaciones, un bit dice si hay escapes.
  - **Codec 1:** la diferencia de claves en varint de nibbles.
- **Claves sin orden** (la primera clave de cada palabra en `Fields` y las entradas de una cabeza): se escriben como `origen − origen base` en `wOrigin` bits e `ID − ID base` en `wID` bits. El ID base es el menor de todos los orígenes, así que los IDs de todas las tablas comparten un rango.
- **Con un solo origen** no hay escapes y `wOrigin = 0`: cuesta exactamente lo mismo que antes (medido: mismos bytes).

### 15.3 Medición

Mismo eval que 13 (`GroupingNone`), con `GXS_EVAL_ORIGINS=n`: cada documento cae al azar en uno de n orígenes, y cada origen tiene sus propios IDs. Con IDs densos, cada origen va de 1 a N/n (un autoincremental por tabla); con IDs aleatorios, `uint32` al azar. Es el caso pesimista: en tablas reales una palabra suele aparecer en pocas.

Bytes promedio por consulta en frío, 1 · 2 · 3 palabras:

| | 1 origen | 8 orígenes | 50 orígenes |
|---|---|---|---|
| 10k, densos | 1,7 · 3,0 · 4,5 KB | 2,0 · 3,5 · 5,3 KB | 2,1 · 3,8 · 5,7 KB |
| 10k, aleatorios | 2,7 · 5,0 · 7,8 KB | 2,8 · 5,3 · 8,2 KB | 2,9 · 5,4 · 8,5 KB |
| 100k, densos | 3,7 · 7,7 · 12,4 KB | 3,9 · 8,2 · 13,3 KB | 4,1 · 8,8 · 14,2 KB |
| 100k, aleatorios | 4,9 · 10,8 · 17,8 KB | 5,0 · 11,3 · 18,7 KB | 5,2 · 11,7 · 19,4 KB |

| 100k | `.idx` total | `.db` total |
|---|---|---|
| densos, 1 · 8 · 50 orígenes | 430 · 462 · 481 KB | 3,91 · 4,93 · 5,73 MB |
| aleatorios, 1 · 8 · 50 orígenes | 560 · 582 · 591 KB | 9,90 · 10,82 · 11,58 MB |

Qué dicen:
- **Las rondas y los requests no cambian.** Solo crecen los bytes.
- **Con IDs aleatorios**, 8 orígenes cuestan un 5 % más y 50 orígenes un 9 %. Ahí el escape no ayuda: un ID al azar ocupa 32 bits igual, y gana el codec 0 simple o el 1.
- **Con IDs densos por origen** (el caso de varias tablas con autoincremental), sin escapes ni claves partidas costaba un 14 % y un 26 % más a 100k, y un 38 % y un 60 % a 10k. Con 15.2 queda en un 7 % y un 15 % a 100k, y en un 18 % y un 27 % a 10k.
- **El ranking es el de la fuerza bruta** en 3 000/3 000 con cualquier cantidad de orígenes, y `TestMatchesBruteForce` mezcla 5 orígenes con IDs repetidos entre orígenes.

---

## 16. Palabras de una letra y stopwords

> Agregado el 2026-10-05. **Todo bucket de letras es una combinación de 2 letras.** Una palabra de una letra no tiene clave ni bucket propio: es la **continuación** de las 2 palabras anteriores más cercanas del documento. Las stopwords se pueden conservar con un flag.

### 16.1 Continuaciones

- **Al indexar** (`key5.FromTokens`): una palabra de una letra se guarda como continuación de **las 2 anfitrionas distintas más cercanas antes de ella** (`key5.MaxContinued`; anfitriona es una palabra de 2 letras o más; los números y las otras letras no lo son). "Polo rojo M" guarda `polo`, `rojo`, `rojo›m` y `polo›m`; "Polo rojo azul M", solo `azul›m` y `rojo›m`. Una anfitriona repetida cuenta por su aparición más reciente ("polo rojo azul polo M" → `polo›m`, `azul›m`). Si no hay ninguna palabra antes ("M polo rojo"), la letra no se guarda.
- **La clave** `anfitriona›letra` son las primeras 6 unidades de la anfitriona, la unidad 0 como marca y la letra (`key5.Continuation`). Queda en el bucket de la anfitriona, justo después de su clave (`talla` < `talla›m` < `tallaa`). Dos anfitrionas que comparten 6 letras comparten la continuación (`refrigerante` y `refrigerado` → `refrig›x`).
- **Posiciones:** las continuaciones de una letra ocupan posiciones consecutivas desde la de la letra, de la anfitriona más cercana a la más lejana. Las palabras siguientes se corren.
- **Formatos:** no cambian. `Key.Len()` cuenta hasta la última unidad distinta de 0, así que los streams de claves llevan la continuación entera. El stream de tokens acepta la unidad 0 solo como marca, antes de la última unidad.
- **Al consultar** (`Parse`): una letra va detrás de las palabras de la consulta, estén donde estén (`m talla` es `talla m`), y busca su continuación con **cada palabra anfitriona de la consulta**: basta una. Sin anfitriona en la consulta, la letra se descarta (`m` sola no encuentra nada). Nunca es prefijo (`m*` es `m`). La palabra abierta sigue siendo la última que se escribió (`m tal`: `tal` es prefijo).
- **Ningún otro término ve una continuación:** `talla›m` no es prefijo ni typo de `talla`, y no suma a su IDF.
- **Lectura:** la continuación está en el `.idx` de la anfitriona, que la consulta ya lee. Antes `talla m` leía dos `.idx` (`ta` y `m0`); ahora uno.

### 16.2 Stopwords

`Config.KeepStopwords` / `ClientOptions.KeepStopwords` / `gxs -keep-stopwords`: el normalizador conserva las stopwords del español (`a`, `de`, `la`, `y`…), que por defecto se descartan. Con el flag, "Salón A" guarda `salon›a`. El flag entra en `NormID`: un buscador con otro valor rechaza la colección, y cambiarlo exige `Rebuild`.

### 16.3 Medición

Mismo eval que 13. En `productos.csv`, el 36 % de los nombres tiene una letra suelta: casi siempre una unidad (`g` 2 434 veces, como en "500 g"; `l` 516; `d` 248). Hay 3 891 letras; pegadas a todas las anfitrionas anteriores dan 19 049 postings de continuación, y pegadas solo a la más cercana, 3 877.

Bytes promedio en frío, 1 · 2 · 3 palabras, y `.db` total:

| | Antes (bucket `x0`) | Solo la anfitriona más cercana | Las 2 más cercanas (lo implementado) | Todas |
|---|---|---|---|---|
| 10k, aleatorios | 2,7 · 5,0 · 7,8 KB | 2,9 · 5,2 · 8,1 KB | 3,0 · 5,5 · 8,5 KB | 3,4 · 6,0 · 9,3 KB |
| 100k, aleatorios | 4,9 · 10,8 · 17,8 KB | 5,1 · 10,7 · 17,9 KB | 5,5 · 11,3 · 18,9 KB | 6,4 · 12,4 · 20,7 KB |
| 100k, densos | 3,7 · 7,7 · 12,4 KB | 3,8 · 7,7 · 12,4 KB | 4,1 · 8,2 · 13,2 KB | 4,5 · 8,8 · 14,3 KB |
| `.db`, 100k aleatorios | 9,90 MB | 9,19 MB | 9,71 MB | 12,16 MB |

- Hay 356 buckets en vez de 379: desaparecen los 23 de una letra.
- El ranking es el de la fuerza bruta en 3 000/3 000.
- **Con todas las anfitrionas** la consulta en frío lee un 16 % más a 100k (un 30 % con 1 palabra): las continuaciones engordan el `.idx` de las palabras comunes. **Con solo la más cercana** cuesta lo mismo que antes, pero "Polo rojo M" ya no se encuentra con `polo m` por la letra (sí por `rojo m`).
- **Se eligió las 2 más cercanas** (2026-10-05): un 4–5 % más que antes en frío a 100k, y la letra sigue a las dos palabras que suelen describirla ("polo rojo M", "vitamina C 500 mg"). Una palabra más lejana encuentra el documento por sí misma, no por la letra.

---

## 17. Grupo de texto y ranking por puntos

> Agregado el 2026-10-05. Un documento puede tener **un grupo**: un texto padre que comparten muchos documentos ("Juan Carlos" vive en "centro civico"; "Pedro Ignacio" también). El grupo agrega especificidad, pero el nombre importa más: "centro civico" como nombre de un documento pesa más que "centro civico" como grupo. El ranking pasa a ser **solo por puntos** (17.5): ya no hay niveles de cobertura.
>
> No confundir con el origen del documento (15), que hasta el 2026-10-05 se llamaba grupo.

### 17.1 Qué se indexa

`Doc{OriginID, ID, Text, Group}`. Del grupo, normalizado igual que el texto (sin stopwords, salvo `KeepStopwords`), salen dos cosas:

- **La firma:** una clave con **4 segmentos de 2 letras** como máximo (las 8 unidades de una clave), un segmento por palabra:
  - primera palabra: su 1.ª y 2.ª letra, así la firma queda en el bucket de esa palabra, que la consulta ya lee;
  - las otras: su 1.ª y 3.ª letra;
  - una palabra de 2 letras da sus 2 letras; una de 1 letra, la unidad 0 y la letra; un número, las 2 primeras unidades de su clave;
  - las palabras de 1 letra al comienzo no cuentan (la firma nunca empieza con 0);
  - **un grupo de una sola palabra usa la palabra misma** (sus 8 primeras letras, o la clave del número): "providencia" → `providen`, no `pr`. Con 2 letras chocaría con cualquier palabra de la consulta que empiece igual ("producto" leería la lista de todos los miembros de "providencia"). Sigue en el bucket de la palabra y en el espacio de firmas, así que el índice sabe que es un grupo y no un nombre: "producto" busca `producto` en ese espacio, que no es `providen`.

  "centro civico" → `cecv`; "condes vuelan alto" → `coveat`; "centro civico norte" → `cecvnr`; "Las Condes" (sin stopwords) → `condes`. Todos los miembros del grupo tienen la firma: es una palabra más del documento, con su lista.
- **Las palabras de grupo:** cada palabra del grupo de 2 letras o más (o número), con su clave de 8 letras, pero **solo para los 100 miembros de clave menor de cada grupo** (`Params.GroupWordDocs`). Son el camino para encontrar el grupo por una sola de sus palabras o por prefijo ("juan civ…"), sin repetir "centro" en la lista de cada miembro.

### 17.2 Espacios de claves

Una firma o una palabra de grupo no pueden confundirse con una palabra: `cecv` podría ser una palabra, y el prefijo "ce" de una consulta la tocaría. Las claves tienen 3 espacios, y **la marca está fuera de las 8 unidades**:

| Espacio | Qué | En memoria (`key5.Key`) | En el stream de tokens | En el `.idx` |
|---|---|---|---|---|
| 0 | palabras (lo de siempre) | bits 40–41 = 0 | header de siempre | entradas del directorio con espacio 0 |
| 1 | palabras de grupo | bits 40–41 = 1 | unidad 30 antes del token | espacio 1 |
| 2 | firmas | bits 40–41 = 2 | unidad 31, el largo y las unidades | espacio 2 |

- **En memoria** es un bit alto de la clave de 64 bits, dentro de una invocación: se reconstruye en cada lectura desde lo que está en S3 (la Lambda no guarda estado). El orden de las claves queda: palabras, después palabras de grupo, después firmas. Por eso un rango de prefijo de palabras nunca incluye las otras.
- **`.idx`:** cada entrada del directorio de `Keys` (`slot u16 | claves u32`) lleva el espacio en los 2 bits altos del slot (un slot ocupa 10 bits). Las entradas van en el orden de las claves: espacio, después slot.
- **Streams de claves de cualquier bucket** (directorio de combinaciones y bloques, 2.3): un bit inicial; con 1, siguen las claves de espacio 1 y 2 que hay al final (varint de nibbles cada una).
- Formatos: `.idx` y `.db` pasan a `Format = 4`. El stream de tokens de `p.log` y `tokens` agrega los headers 30 y 31; uno sin grupos se lee igual que antes.

Lo demás no cambia: listas, cabezas, combinaciones, expansión y T tratan a una firma y a una palabra de grupo como a cualquier clave. Un grupo de 5 000 miembros tiene una firma con una lista grande, que se expande y tiene cabeza y combinaciones (`juan·cecv`) como cualquier palabra frecuente. **La cabeza de una firma o de una palabra de grupo se ordena solo por clave**, porque su puntaje no depende de la posición (17.5).

### 17.3 Consulta

La consulta no sabe qué palabras son el grupo. Por cada tramo de 1 a 4 palabras seguidas (que no empiece con una palabra de 1 letra) arma la firma y la busca en el bucket de la primera palabra del tramo, **que ya lee**: **0 requests de más**.
- Un tramo de 2 segmentos o más también encuentra las firmas más largas que empiezan igual (`cecv` → `cecvnr`), con menos puntos (17.5). Un tramo de 1 palabra es la palabra misma y solo encuentra la firma igual: "condes" encuentra el grupo "Las Condes" y nada más. Por prefijo ("provi…") se encuentra el grupo por sus palabras de grupo.
- La palabra abierta (la última) entra en un tramo de varias palabras solo con 3 letras o más (su segmento tiene que estar completo), y sola con 2 o más.
- Cada palabra de la consulta busca además su palabra de grupo (exacta y, si es la abierta, por prefijo), en el mismo bucket.
- Las firmas de los tramos son **términos virtuales**: no suman cobertura propia, no tienen IDF ni bonus de posición ni de par.

Las ops de `p.log` aplican sus palabras de grupo aunque el documento no esté entre los 100, y una colección sin índice (`PLAN_5BIT.md` 5.8) no tiene tope: el tope lo decide la compactación cuando construye los buckets.

### 17.4 Escritura

`Apply` y `Rebuild` agregan los tokens del grupo al final de los del texto: las palabras de grupo (header 30) y la firma (header 31). La compactación decide el tope: recorre los documentos en orden de clave y, por cada grupo (la secuencia de sus claves de grupo y su firma), conserva las palabras de grupo de los primeros 100. Un miembro nuevo con clave mayor no cambia ese conjunto, así que no obliga a reescribir el bucket.

### 17.5 Ranking por puntos

Los resultados se ordenan **por puntaje** y después por clave. La cobertura se sigue informando (`Hit.Coverage`: términos cubiertos por el nombre o por el grupo), pero no ordena. Constantes en `query/score.go`:

| Constante | Valor | Qué |
|---|---|---|
| `LetterFirst` | 5 | la primera letra coincide |
| `LetterNext` | 3 | cada letra siguiente que coincide, hasta la 8.ª (un número cuenta dígitos) |
| `WordFull` | 7 | palabra completa: misma clave y misma cola |
| `LetterExtra` | 3 (= `LetterNext`) | cada letra de diferencia con una palabra cercana, "bolsa" para "bolsas" (19) |
| `GroupSegment` | 4 | cada segmento de la firma que coincide |
| `GroupFull` | 2 | la firma entera coincide |
| `GroupWord` | 4 | cada término que coincide solo por una palabra de grupo |
| `PairAdjacent` · `PairAdjacentReversed` · `PairNear` | 6 · 3 · 2 | dos términos del nombre juntos y en orden · juntos al revés · a una palabra |
| `DocFirstWord` · `DocSecondWord` | 2 · 1 | la palabra es la primera · la segunda del documento (la primera valía 3 hasta el 2026-10-05) |
| `QueryFirstWord` | 2 | la palabra es la primera que escribió el usuario, que suele guiar la intención ("cafe organico" busca café). Se suma a la de arriba: la primera de la consulta que también es la primera del documento gana 4 |
| `IDFWeight` · `IDFMax` | 2 · 8 | rareza: `round(2 · ln(N/df))`, solo términos del nombre. El tope era 15; el benchmark (`PLAN_BENCH.md` 11) mide mejor con 8: toda palabra en menos del 1,8 % de los documentos ya es rara |

- **Nombre:** exacta = 5 + 3·(n − 1) + 7 con n letras (máximo 8): "juan" 21, "carlos" 27, "maximiliano" 33. Raíz (misma clave, otra cola): sin el +7. Prefijo: las letras escritas, sin el +7 ("carl" = 14). Typo: las letras menos la distancia, sin el +7 ("carlso" = 17). Una letra suelta (continuación) = 5 + 7.
- **Grupo:** `max(firma, palabras de grupo)`. La firma da 4 por segmento de la consulta y +2 si la firma del documento es exactamente esa ("centro civico" completo = 10; contra "centro civico norte" = 8; un grupo de una palabra, como "condes", = 6). Las palabras de grupo dan 4 por término que solo coincide por ellas. Un término que coincide por el nombre no suma por palabra de grupo.
- **Nombre y grupo a la vez (2026-10-05):** si la consulta tiene palabras fuera de la corrida de la mejor firma del documento, es un nombre más un grupo, y las palabras de la corrida cuentan **una vez**: el puntaje es `max(nombre solo, nombre fuera de la corrida + grupo)`. Para "hidratante cuidado corporal", "Crema corporal @ cuidado corporal" ya no gana "corporal" por el nombre y por el grupo, y "Crema hidratante @ cuidado corporal" le gana (antes empataban). Si la firma cubre toda la consulta ("limpieza facial"), nombre y grupo suman los dos: así un miembro cuyo nombre también lo dice va primero, que es lo que piden las consultas de grupo. El grupo nunca baja un puntaje. Contar una vez también en ese caso costaba 0,07 de nDCG en `grupo` (`PLAN_BENCH.md` 15).
- **Palabras repetidas:** una palabra escrita dos veces es un solo término ("platos preparados preparados"); las corridas de las firmas conservan las dos.
- **Se eliminaron:** el nivel de cobertura, el puntaje base por palabra, los puntos fijos de prefijo, raíz y typo, el bonus de posición en la consulta (volvió el 2026-10-05, solo para la primera palabra: `QueryFirstWord`), el de frase y la mitad del par con typo.

"juan carlos centro civico":

| Doc | Nombre | Grupo | Par | Posición | Total |
|---|---|---|---|---|---|
| Juan Carlos @ centro civico | 21 + 27 | 10 | 6 | 2 + 1 + 2 | **69** |
| edificio "Centro Civico" | 27 + 27 | — | 6 | 2 + 1 | **63** |
| Juan Carlos @ los condes | 21 + 27 | — | 6 | 2 + 1 + 2 | **59** |
| Juan @ centro civico norte | 21 | 8 | — | 2 + 2 | **33** |
| Pedro @ centro civico | — | 10 | — | — | **10** |

(sin IDF, que depende del corpus. Posición: primera y segunda del documento, y +2 de "juan", la primera palabra escrita.)

**Exactitud:** la cabeza de una palabra sigue siendo el top exacto de esa palabra sola, porque su puntaje solo depende de la clase de coincidencia y de la primera posición (3.4); el bonus de primera palabra de la consulta es el mismo para toda la lista del término. La de una firma o de una palabra de grupo tiene un puntaje fijo, y por eso va ordenada por clave. La verificación de 6.1 (paso 7) usa esas mismas cuentas.

### 17.6 Medición

Eval de 13 con `GXS_EVAL_TEXTGROUPS=300`: 2 de cada 3 documentos tienen uno de 300 grupos sintéticos (comunas de Chile, solas o con un sector: "Providencia", "Las Condes Norte", "Maipú Centro Cívico"), con pocos grupos que se llevan casi todos los documentos. Las "consultas de grupo" son una consulta de 2 palabras del nombre más el grupo; se mide en qué lugar queda el documento con y sin el grupo en la consulta.

Consultas de 1 · 2 · 3 palabras que no nombran grupos, en frío, 100k IDs aleatorios:

| | requests | bytes |
|---|---|---|
| sin grupos | 2.95 · 4.99 · 7.55 | 5.5 · 11.3 · 18.9 KB |
| grupo de 1 palabra con firma de 2 letras | 3.36 · 5.96 · 9.38 | 7.0 · 14.0 · 24.0 KB |
| **grupo de 1 palabra con firma = la palabra** | **2.95 · 5.04 · 7.64** | **5.6 · 11.6 · 19.9 KB** |

Con la firma de 2 letras, "producto" leía la lista de todos los miembros de "Providencia". Con la palabra, el costo queda casi como sin grupos.

Consultas de grupo (firma = la palabra), lugar medio del documento y veces que queda primero:

| Corpus | Con el grupo | Sin el grupo | Requests en frío · bytes |
|---|---|---|---|
| 10k densos | 1.2 · 869/950 primero | 7.2 · 447 | 4.55 · 7.3 KB |
| 10k aleatorios | 1.3 · 817/914 | 8.8 · 411 | 6.45 · 11.8 KB |
| 100k densos | 3.1 · 1302/1500 | 11.9 · 453 | 8.22 · 18.1 KB |
| 100k aleatorios | 4.1 · 1055/1500 | 33.3 · 68 | 10.31 · 24.6 KB |

Con la firma de 2 letras el lugar medio a 100k aleatorios era 4.8 (954 primero), y sin firma para los grupos de una palabra 20.4 (593).

## 18. Benchmark estándar

> Agregado el 2026-10-05. El detalle, las decisiones y el uso están en `PLAN_BENCH.md`. Es la medida principal de efectividad; el eval de 13 sigue para el costo a 100k.

10 010 productos de un catálogo real, 315 categorías como grupos, 200 consultas fijas (exactas, parciales, escribiendo, typos, tildes, marcas, números, nombre+grupo, grupo y negativas) con juicios de relevancia de una regla sobre el texto completo, y dos referencias: BM25 (SQLite FTS5) y BM25 de trigramas. `GXS_BENCH=1 go test ./internal/query -run 'TestBench$' -v` (5 s) escribe `testdata/bench/report.md` y falla si algo empeora respecto de `baseline.json`.

Resultado (IDs de fila, `es100`; B = con la categoría como grupo):

| | S@1 | MRR | nDCG@10 | falsos | GET en frío (prom.) | KB en frío (prom.) |
|---|---|---|---|---|---|---|
| A: nombres, genix-s3 | 0,72 | 0,78 | 0,90 | 0,03 | 3,8 | 6,7 |
| A: BM25 | 0,63 | 0,72 | 0,86 | 0,03 | | |
| B: nombres + grupos, genix-s3 | 0,78 | 0,85 | 0,92 | 0,03 | 3,9 | 7,8 |
| B: BM25 | 0,72 | 0,81 | 0,91 | 0,02 | | |

- Igualamos o superamos a BM25 salvo en las consultas que son solo el nombre de una categoría (`grupo`: nDCG 0,60 contra 0,80), porque el nombre pesa más que el grupo (17).
- Los typos en las 2 primeras letras no se corrigen (6 de 25: S@1 0,17; los otros 19, 0,89).
- El grupo lleva el S@1 de las consultas `nombre+grupo` de 0,16 a 0,52 y cuesta +1,1 KB (p50) en las consultas que no lo nombran.
- Con IDs aleatorios el `.db` pasa de 4 KB a 395 KB y el frío de 3,8 a 4,6 GET.
- Decisiones de la fase 5: `IDFMax` 15 → 8 (17.5), que mejora sin costo; firma de 1.ª+3.ª letra y presupuesto de prefijos (32 KB) se quedan.

## 19. Palabras cercanas: singular y plural

> Agregado el 2026-10-05. Antes "bolsas" no encontraba "Bolsa": la clave son las 8 primeras letras, la raíz (misma clave, otra cola) solo existe pasadas las 8, el prefijo solo vale para la última palabra y solo hacia adelante ("bols" → "bolsas"), y los typos solo se buscan si la palabra no existe en el diccionario.

**Regla.** Una palabra del documento es **cercana** a un término cuando tiene sus mismas letras con **una letra más o menos al final**, o con **"es" más o menos** ("bolsa"/"bolsas", "galleta"/"galletas", "pan"/"panes", "color"/"colores"). La más corta tiene 3 letras o más. Dos letras que no son "es" no cuentan: "sal"/"salsa", "bot"/"boton", "uni"/"unica", "smile"/"smileat" eran falsos en el benchmark.

- Vale para **toda palabra de la consulta**, sea la última o no, exista o no en el diccionario, con o sin `Fuzzy`. No vale para números, `foo*`, palabras de grupo ni firmas.
- Una palabra del documento de 8 letras con cola (sigue después de la 8.ª) no es cercana de un término de 6 o 7: la diferencia es de 2 letras o más y no se sabe cuáles.
- **Puntaje:** las letras que comparten, menos `LetterExtra` (= `LetterNext`, 3) por letra de diferencia, sin el +7 de palabra completa. "bolsas" contra "Bolsa" = 17 − 3 = 14, igual que el prefijo "bols"; la palabra exacta, 27. Con `LetterExtra` 1, 2, 3 o 5 el benchmark da lo mismo.
- **Costo:** la palabra cercana está en el bucket del término, cuyo `.idx` la consulta ya lee: **0 requests más**. Su lista se lee dentro del presupuesto de prefijos, la más cercana primero; una palabra expandida de menos de 8 letras se lee por su cabeza, porque todos sus postings coinciden igual (sin cola). La CPU de recorrer el `.idx` no importa.
- Cuando el término no tiene palabra exacta, las cercanas cuentan como encontradas: ya no se buscan typos.

Código: `Term.nearKey` y `nearPoints` (`query/score.go`), `readPlan.addNear` (`query/plan.go`), `headBounds` (`query/assemble.go`). Test: `TestNearWords`.

**Medición** (benchmark con la categoría `plural` y el juez que acepta singular y plural, `PLAN_BENCH.md` 14; IDs de fila):

| | S@1 A · B | nDCG@10 A · B | falsos A · B | `plural` S@1 · nDCG (B) | GET · KB en frío (B, prom.) |
|---|---|---|---|---|---|
| sin palabras cercanas | 0,70 · 0,75 | 0,897 · 0,912 | 0,018 · 0,022 | 0,56 · 0,85 | 3,83 · 7,8 |
| **1 letra, o "es"** | **0,75 · 0,78** | **0,926 · 0,929** | 0,018 · 0,024 | **0,80 · 0,98** | 3,84 · 7,8 |
| 1 o 2 letras cualesquiera | 0,74 · 0,77 | 0,925 · 0,928 | 0,023 · 0,029 | 0,80 · 0,98 | 3,84 · 7,8 |
| solo 1 letra | 0,74 · 0,77 | 0,920 · 0,927 | 0,018 · 0,024 | 0,76 · 0,96 | 3,84 · 7,8 |

Con IDs aleatorios: nDCG 0,900 → 0,928 (A) y 0,915 → 0,930 (B), GET 4,95 → 4,97 (B). BM25 en `plural`: nDCG 0,81.
