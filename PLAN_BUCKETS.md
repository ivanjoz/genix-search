# Plan: índice por buckets (`.idx` + `.db`) sobre S3 Express

> Estado: **diseño oficial, en implementación** (2026-10-03). Reemplaza las secciones 5 a 8 de `PLAN_5BIT.md` (formatos, ruta de consulta, escritura): el diccionario global `k.<gen>`, los postings `p.<gen>`, el snapshot, el manifest y las generaciones desaparecen. **Siguen vigentes** las secciones 2 a 4 de `PLAN_5BIT.md`: el alfabeto de 5 bits, la clave de 8 letras con su cola de 2 bits, los números `NUM_n`, el normalizador y `NormID`.
> **Qué se optimiza**, en orden: los requests a S3 por consulta, después los bytes leídos por consulta. El tamaño de los archivos no es el objetivo. No se supone nada del corpus ni de los IDs (`uint32` arbitrarios).

## 0. Resumen

- Una partición es `p.log`, un par `<xy>.idx` / `<xy>.db` por cada par de primeras letras (bucket) que exista, y `tokens`, que solo lee la compactación.
- **El nombre del índice de una palabra sale de la consulta**: "camisa" está en `ca.idx`. Por eso la ronda 1 pide `p.log` y los `.idx` en paralelo, sin directorio, sin generación y sin manifest.
- **El `.idx` es un árbol** que se vuelve más específico donde hay densidad: todas las palabras del bucket con la posición exacta de su lista, las listas chicas dentro del propio `.idx`, y para los nodos grandes una **cabeza** (los mejores productos) y una **expansión por la segunda palabra** (`camisa·roja`), y por la tercera si la combinación también es grande.
- **La ronda 2 lee solo rangos exactos del `.db`**: la lista de una palabra o la de una combinación. Muchas consultas no tienen ronda 2: lo que necesitan está en el `.idx`.

| Consulta en frío | GET | Rondas |
|---|---|---|
| 1 palabra con la lista o la cabeza en el `.idx` | 2 | 1 |
| 1 palabra con la lista en el `.db` | 3 | 2 |
| N palabras | 1 + buckets distintos + rangos del `.db` | 2 |
| Colección chica (sin índice) | 2 (el `.idx` da 404) | 1 |
| En caliente (`.idx` en caché) | 1 + rangos del `.db` | 1 |

Semántica de resultados: la de `PLAN_5BIT.md` 13.3, **O con niveles estrictos**: primero por cuántas palabras de la consulta tiene el producto (cobertura), después por puntaje, después por ID.

---

## 1. Objetos de una partición

```
<root><col>/<part>/p.log          cambios pendientes                    se agrega al final; la compactación lo reescribe (If-Match)
<root><col>/<part>/<xy>.idx       índice del bucket xy                  se sobrescribe cuando el bucket cambia
<root><col>/<part>/<xy>.db        listas del bucket xy (si hacen falta) se sobrescribe entero, antes que su .idx
<root><col>/<part>/tokens         palabras de cada producto             solo la compactación
<root><col>/<part>/compact.lease  exclusión entre compactaciones        If-None-Match, con vencimiento
```

- **Nombre del bucket:** las dos primeras unidades de la clave, cada una con un carácter de `0abcdefghijklmnopqrstuvwxyz12345` (unidad 0 = `0`, 1–26 = `a`–`z`, 27–31 = `1`–`5`). `camisa` → `ca`; la palabra de una letra `x` → `x0`; el número 50 (`NUM_2`, unidad alta 1) → `2a`.
- **`<xy>.db` solo existe si alguna lista del bucket no entra en el `.idx`.** A 10k productos son unos 13 de unos 380 buckets.
- Una colección con menos de `MinIndexDocs` documentos (100) no tiene `.idx`, `.db` ni `tokens`: todo está en `p.log` (`PLAN_5BIT.md` 5.8, sin cambios).

---

## 2. Piezas comunes

Reglas de `PLAN_5BIT.md` 5.0: solo bytes, little‑endian, streams de bits LSB‑first, CRC32C, `magic` + `Format` en cada header.

### 2.1 Listas de postings (c columnas)

Una lista tiene n entradas ordenadas por `(ID, pos₁, …, pos_c)`. `c = 1` para una palabra, 2 para una combinación de dos palabras, 3 para tres. Un producto con la palabra en dos posiciones tiene dos entradas.

```
diferencias de ID   n − 1 valores: W bits fijos (Codec 0) o varint de nibbles (Codec 1)
posiciones          por columna: n × 4 bits (saturadas en 15)
colas               por cada columna con Tail = 1: n × 2 bits
```

El descriptor de la lista va en el diccionario que la apunta: `n`, primer ID, `Codec`, `X` (W o cantidad de nibbles) y un bit `Tail` por columna. Con eso el largo en bits se calcula, así que nunca se guarda. Con `c = 1` es exactamente el formato de `PLAN_5BIT.md` 5.2.

### 2.2 Stream de claves

- **Claves de un bucket:** el formato de `PLAN_5BIT.md` 3.2 (cabecera de una unidad `(s, L)` y las letras nuevas, después de las dos del bucket).
- **Claves de cualquier bucket** (las segundas y terceras palabras de una combinación): por clave, `s` (3 bits, unidades compartidas con la anterior), `L − 1` (3 bits, unidades de la clave) y las `L − s` unidades nuevas de 5 bits.

---

## 3. `<xy>.idx`

```
Header (64 B)
  0   magic "GXI1"
  4   Format       uint16
  6   ETagLen      uint16     largo del ETag del .db
  8   NormID       uint32
  12  Epoch        uint32     época del índice (7)
  16  Seq          uint64     la compactación que lo escribió: incluye las ops ≤ Seq
  24  Slot         uint16
  26  reservado    uint16
  28  Keys         uint32     claves del bucket (0 = bucket vacío, una lápida)
  32  DBSize       uint32     0 = no hay .db
  36  DBCombos     uint32     dónde empieza la zona de combinaciones del .db
  40  KeysLen, FieldsLen, HeadsLen, ExpLen, InlineLen   5 × uint32: largo de cada sección
  60  CRC32C       uint32     del header (0..60) y de todo lo que sigue
ETag     [ETagLen]B   ETag que S3 devolvió al escribir el .db
Keys     las claves del bucket, ordenadas (2.2)
Fields   wCount u8 | wDup u8 | wFirst u8 | wX u8 | base u32, y por clave, en bits:
           count[wCount] | dup[wDup] | first−base[wFirst] | V[2+wX] | loc[2]
Heads    cabezas de prefijo (3.2)
Exp      expansiones (3.3), una por palabra expandida, en orden de clave
Inline   las listas en línea, pegadas en bits en orden de clave
```

### 3.1 Campos de cada palabra

- `count`: entradas de la lista. `dup = count − df`, con `df` los productos distintos (casi siempre 0).
- `V`: para `count = 1`, la posición y la cola del único posting (`pos | cola << 4`); si no, `Tail | Codec << 1 | X << 2` (2.1).
- `loc`:
  - 0 = un solo producto: todo está en el campo, no hay lista;
  - 1 = lista **en línea**, en la sección `Inline`;
  - 2 = lista en la zona de palabras del `.db`;
  - 3 = lista en el `.db` y además **expandida** (tiene registro en `Exp`).
- **La posición de cada lista no se guarda:** es la suma de los largos de las anteriores con el mismo `loc` (las de `Inline` desde el principio de la sección; las del `.db` desde el byte 32 del `.db`).
- Un prefijo es un tramo contiguo de claves, así que sus listas son un tramo contiguo de la zona de palabras: un prefijo se lee de **un solo rango**.

### 3.2 Cabezas de prefijo

Para cada nodo de letras (un prefijo de 2 a 7 unidades que cubre al menos 2 claves) cuyas listas pasan de T bytes (5):

```
n uint16, y por cabeza (alineada a byte):
  from uint32 | to uint32   tramo de claves [from, to] del nodo
  df uint32                 productos distintos del tramo
  bloque de cabeza          (3.4), entradas = (ID, primera posición de cualquier clave del tramo)
```

Dos prefijos que cubren las mismas claves comparten la cabeza.

### 3.3 Expansión de una palabra

```
key uint32                       índice de la clave
grupos de cabeza:
  g uint8, y por grupo: cola uint8 (255 = todas) | bloque de cabeza (3.4)
diccionario de combinaciones (3.5)
```

- **Grupos de cabeza:** el grupo "todas" ordena por la primera posición de la palabra en el producto. Si la clave tiene dos colas o más (`chocolat` de `chocolate` y `chocolatera`), hay además un grupo por cola, ordenado por la primera posición con esa cola. Así la cabeza da el top exacto para cualquier cola de la consulta: los productos con la cola de la consulta (palabra exacta) salen del grupo de esa cola, y los demás (misma raíz) salen del grupo "todas".
- **Entradas:** cada una lleva la máscara de colas presentes y la primera posición de cada una, para que el lector calcule el puntaje exacto.

### 3.4 Bloque de cabeza

```
m uint8 | flags uint8 (bit 0 = con máscara de colas) | wID uint8 | reservado | base uint32
entradas, en bits: ID−base[wID] | máscara[4] (si el flag) | pos[4] por cola presente (una sola sin máscara)
```

- **Orden:** por bonus de posición (primera palabra, segunda, resto; 13.3 de `PLAN_5BIT.md`) y después por ID ascendente. Es el orden del puntaje de un término, así que las primeras m entradas son el top exacto del nodo para una consulta de ese término solo.
- `m` ≤ H = 64.

### 3.5 Diccionario de combinaciones

Las combinaciones de una palabra expandida `a` son las listas de los productos que tienen `a` y otra clave `k`: columnas `(pos de a, pos de k)` y sus colas.

- **Dónde se guarda el par {a, k}:** bajo la palabra expandida. Si las dos lo están, bajo la clave menor. El lector lo sabe después de la ronda 1, porque leyó los dos `.idx`.

```
n uint32 | p uint8 | wDF uint8 | wBytes uint8 | reservado
claves     n claves (2.2), ordenadas
campos     por entrada, en bits: exacta[1] | expandida[1] | df[wDF] | bytes[wBytes]
triples    por cada entrada expandida, en orden: su diccionario de terceras palabras (mismo formato, sin expandidas)
```

- **Entrada exacta:** una sola clave `k`.
- **Entrada de grupo:** cubre las claves cuyas primeras `p` unidades son la clave de la entrada y que no tienen entrada exacta.
- **Elección de `p`:** es 8 (todo exacto) salvo que el diccionario pase de `ComboDictBudget` (2 KB). En ese caso se toma el `p` más grande que entre: es el "baja otra letra" del diseño. Una combinación expandida siempre es exacta.
- **Bloques en el `.db`:** cada entrada tiene un bloque, en el orden de las expansiones y de las entradas, con los bloques de triples después del bloque de su par. Las posiciones salen de sumar `bytes`.
- **Combinación expandida:** un par `(a, k)` cuya lista pasa de T. Tiene un diccionario de terceras palabras `j`, con listas de 3 columnas.

---

## 4. `<xy>.db`

```
Header (32 B): magic "GXD1" | Format uint16 | Slot uint16 | NormID uint32 | Epoch uint32 | Seq uint64
               | CRC32C del cuerpo uint32 | reservado uint32
Zona de palabras        las listas con loc 2 o 3, pegadas en bits en orden de clave, alineadas a byte al final
Zona de combinaciones   bloques de combinación, cada uno alineado a byte
```

Bloque de combinación (autodescripto, porque se lee entero):

```
n uint16 | c uint8 (2 o 3) | wCount u8 | wFirst u8 | wX u8 | reservado uint16 | base uint32
claves   las n claves que cubre (2.2)
campos   por clave, en bits: count[wCount] | first−base[wFirst] | V[c+1+wX] (Tail por columna | Codec | X)
listas   pegadas en bits, en orden de clave
```

El lector **no lee el header del `.db`**, solo rangos. La consistencia la da el ETag (7).

---

## 5. Qué tan específico es el índice: T

Todo se decide por bucket en cada compactación, con D = bytes de todas las listas de palabras del bucket:

> **T = máx(MinSplit, √(ExpandCost × D))**, con MinSplit = ExpandCost = 2 KB.

- **Por qué la raíz:** expandir una palabra agrega ~ExpandCost al `.idx`, y eso lo paga toda consulta que lee ese bucket. El ahorro solo lo reciben las consultas que usan esa palabra, que son más cuanto más frecuente es. El equilibrio entre ambos da la raíz. Como mucho se expanden D/T palabras, así que el `.idx` crece ≈ T por las expansiones y la lectura máxima de una lista en el `.db` es ≈ T: índice y datos crecen juntos.
- **Una palabra se expande** si su lista pasa de T. **Histéresis:** una ya expandida se colapsa recién al bajar de T/2. El conjunto de expandidas se guarda en `tokens`.
- **Un nodo de letras tiene cabeza** si sus listas pasan de T (3.2).
- **Una combinación se expande** (a terceras palabras) si su lista pasa de T, con la misma histéresis. Profundidad máxima: 3 palabras.
- **Listas en línea:** las de palabras no expandidas, de la más chica a la más grande, mientras sumen hasta `InlineBudget` (2 KB). Las de un solo producto no cuentan: van en el campo.

| Bucket "ca" (estimado) | D | T |
|---|---|---|
| bucket chico | ~1 KB | 2 KB (no se expande nada) |
| 10k productos | ~6,7 KB | ~3,7 KB |
| 100k | ~69 KB | ~12 KB |
| 1M | ~690 KB | ~37 KB |

---

## 6. Lectura

### 6.1 Rondas

1. **Ronda 1, en paralelo:** `p.log` y el `.idx` de cada bucket que necesitan los términos: el de la palabra; los de su rango de prefijo (los números con prefijo pueden abarcar varios); y para los typos, el mismo bucket, porque **no hay typos en las dos primeras letras**.
2. **Resolver cada término** en su `.idx`:
   - exacto;
   - la última palabra también como prefijo (un tramo de claves);
   - typos (desde la 3.ª letra, edit distance de `PLAN_5BIT.md` 7.4) solo si el término no tiene ni exacta ni prefijo.

   Cada clave resultante es **chica** (lista en el campo, en línea o en el `.db`) o **grande** (expandida).
3. **Plan de lectura.** Las fuentes son de tres tipos:
   - **completas:** las listas de las claves chicas de todos los términos, y las combinaciones de cada clave grande `a` con las claves de cada otro término (las que viven bajo `a`: chicas o mayores que `a`);
   - **parciales:** las cabezas de las claves grandes y de los prefijos grandes, que ya vienen en el `.idx`;
   - **triples, con 3 términos:** si un par `(a, k)` está expandido, se leen sus triples con las claves del tercer término en vez del par entero. Así el nivel 3 sale exacto y chico, y el nivel 2 de ese par queda pendiente.

   Un prefijo grande sin otros términos con claves chicas no lee sus listas chicas: su nivel 1 sale de la cabeza, y sus pares, de las combinaciones de los otros términos.
4. **Ronda 2:** los rangos del `.db` (se fusionan los cercanos), en paralelo. Si todo estaba en el `.idx`, no hay ronda 2.
5. **Armado:**
   - cada producto encontrado en una fuente completa tiene su cobertura exacta: si tiene otro término por una clave grande, está en una combinación;
   - los productos que solo aparecen en una cabeza son de nivel 1 (o de nivel 2 por pares de una clave consigo misma);
   - se aplica el overlay de `p.log`: un producto con op pendiente se saca y se vuelve a evaluar con sus claves nuevas;
   - se puntúa con el ranking de siempre.
6. **Verificación de exactitud.** Cada fuente parcial tiene un umbral: el puntaje de su última entrada, o `(cobertura 2, ∞)` para un par reemplazado por triples. Ningún producto no visto de esa fuente puede superar su umbral. Si el resultado número `offset + limit` no supera todos los umbrales, **ronda 3**: se leen completas las fuentes parciales que quedaron cortas (la lista completa de la clave grande en el `.db`, el par entero), y se arma de nuevo. Pasa en páginas profundas, o cuando casi toda una cabeza tiene también otros términos.

Las cabezas no se usan para typos (su puntaje no depende de la posición): una clave grande con typo lee su lista completa.

### 6.2 IDF

El `df` de un término es la cantidad de documentos del índice (a `LastSeq`) que lo tienen, y N es `Docs` de la base del log:
- exacto: `df` de la clave;
- prefijo con cabeza: `df` del nodo;
- prefijo sin cabeza o typo: la unión de las listas leídas.

Antes el `df` contaba los candidatos después del overlay. Ahora es el del índice: las ops pendientes son pocas (`CompactBytes`), y así no hace falta leer listas enteras para el IDF. La colección sin índice (solo `p.log`) sigue contando sus documentos vivos.

### 6.3 Presupuesto de prefijos

Un prefijo o typo que tiene que leer muchas listas chicas sigue limitado por `PrefixBudget` (32 KB, las más frecuentes primero). Si deja claves afuera marca `Approx`, como en `PLAN_5BIT.md` 7.3.

---

## 7. Consistencia sin generaciones

**Base de `p.log` (Format 2, 176 B):**

```
0   magic "GXL1"   4
4   Format         uint16 = 2
6   flags          uint16       bit 0 = LogOnly
8   NormID         uint32
12  Epoch          uint32
16  LastSeq        uint64       las ops ≤ LastSeq están en el índice (o en Docs, sin índice)
24  PrevSeq        uint64       el LastSeq de la base anterior (caché en caliente, 10)
32  UnixMs         int64        cuándo se escribió
40  Docs           uint32       documentos a LastSeq
44  Touched        [128]byte    bit s = la compactación que escribió esta base reescribió el bucket s
172 CRC32C         uint32
```

Los frames (`Batch`, `Compact`, `Docs`) no cambian.

**Reglas del lector**, al terminar la ronda 1:

1. **`Epoch` distinta** entre el `.idx` y la base: hay una reconstrucción en curso, así que se repite la ronda 1, con backoff y unos segundos como máximo.
2. **`.idx.Seq < LastSeq` con `Touched[s]`:** se leyó la versión anterior a la última compactación, y se repite la ronda 1.
3. **`.idx.Seq ≥ LastSeq`**, o `Seq < LastSeq` sin `Touched[s]`: vigente. Si es más nuevo que el log (la compactación escribió el `.idx` y todavía no recortó el log), es inofensivo: el log se aplica entero, y una op ya incorporada da el mismo producto.
4. **404 con `Touched[s]`:** puede ser un bucket recién creado que S3 todavía no devolvió en esa lectura paralela. Se pide de nuevo una vez. Un bucket que quedó vacío no da 404: deja una lápida (`Keys = 0`).
5. **Cada rango del `.db`** trae el ETag del objeto. Si no es el `DBETag` del `.idx`, el bucket se reescribió entre las rondas y se repite la consulta.
6. **Compactaciones separadas al menos 1 s** (`CompactSpacing`), y una ronda 1 que tarda más de 1 s se repite. Así una lectura no puede ver un `.idx` de dos compactaciones atrás junto con el log actual, que es lo único que la regla 2 no detecta.

**Época:** sube en cada reconstrucción completa (`Rebuild`, `Drop`, y el paso de sin índice a con índice y viceversa). La reconstrucción escribe todos los buckets y borra los demás nombres. Un `.idx` de otra época nunca se mezcla.

---

## 8. Escritura

**Append:** igual que `PLAN_5BIT.md` 8: `Apply` es una lectura y un append, con `CompactBytes` y `MinIndexDocs`.

**Compactación incremental** (`Compact`):

1. **Lease:** `PUT compact.lease` con `If-None-Match` (dueño y vencimiento). Si existe y está vencido, se toma con `If-Match`. Si no, `ErrConflict`.
2. Lee `p.log` y `tokens` en paralelo. Espera hasta `base.UnixMs + CompactSpacing`.
3. Aplica las ops pendientes al estado `ID → claves`.
4. **Bajo `MinIndexDocs`:**
   - recorta `p.log` a una base `LogOnly` con `Docs`;
   - si había índice, sube la época y borra `tokens`, `.idx` y `.db`.
5. **Construye en memoria el índice completo** a partir del estado: listas, D y T por bucket, y expansiones con su histéresis.
6. **Buckets tocados:**
   - los de las claves viejas y nuevas de cada producto que cambió;
   - si cambió el conjunto de expandidas (de palabras o de pares), los de todas las expandidas, viejas y nuevas;
   - si no había índice, todos, con época nueva.
7. **Por cada bucket tocado,** en paralelo:
   - escribe `<xy>.db` (si hace falta) y toma su ETag;
   - después escribe `<xy>.idx` con ese ETag y `Seq = TargetSeq`;
   - un bucket vacío deja una lápida y borra su `.db`.
8. Escribe `tokens`, con el estado y las expandidas.
9. **Recorta `p.log`** con `If-Match`. La base nueva lleva `LastSeq = TargetSeq`, `PrevSeq`, `Docs` y `Touched`, y se conservan las ops y los pedidos posteriores.
10. Borra el lease.

**Una caída a mitad de camino es inofensiva:**
- `.idx` más nuevos que el log valen (7.3);
- si `tokens` quedó viejo, la siguiente compactación vuelve a calcular los buckets tocados con las claves viejas;
- si `tokens` quedó nuevo, los buckets de las claves viejas ya se habían reescrito, porque van antes.

**Costo:** hasta 2 PUT por bucket tocado, más `tokens` y el log. Lo que se reescribe es proporcional a los buckets tocados, no a la colección.

---

## 9. Reconstrucción, vaciado y colecciones chicas

- **`Rebuild`:**
  1. lease;
  2. anota el último `seq` del log, S0, **antes** de leer la fuente;
  3. lee la fuente y construye todo con época nueva;
  4. escribe los buckets y borra los nombres que no usa;
  5. escribe `tokens`;
  6. recorta el log conservando las ops posteriores a S0.

  El razonamiento de `PLAN_5BIT.md` 8 sobre S0 no cambia.
- **`Drop`:** una reconstrucción con la fuente vacía. Deja un `p.log` `LogOnly` sin documentos y borra el índice.
- **`Init`:** una reconstrucción sobre una colección que no existe (`ErrExists` si ya hay `p.log`).
- **Colección chica:** sin cambios. Al pasar a tener índice o al dejar de tenerlo, sube la época.

---

## 10. Caché en caliente

- **Qué se guarda:** el motor guarda los `.idx` ya leídos, cada uno con el `LastSeq` en que se validó.
- **Cuándo sigue valiendo:** si la base del log tiene el mismo `LastSeq`, o si `PrevSeq` es ese valor y el bucket no está en `Touched`.
- **Consulta en caliente:** los rangos del `.db` se piden en la ronda 1, junto con `p.log`, a partir del `.idx` en caché, y se validan después con la base y el ETag. Si algo no vale (raro), se repite en frío.
- **Resultado:** 1 ronda; 1 GET si todo está en el `.idx`.

---

## 11. Parámetros

| Parámetro | Valor | Dónde |
|---|---|---|
| `MinSplit` | 2 KB | mínimo de T (5) |
| `ExpandCost` | 2 KB | costo de una expansión en el `.idx` (5) |
| `HeadSize` (H) | 64 | entradas de una cabeza: 3 páginas de 20 más margen |
| `InlineBudget` | 2 KB | listas en línea por `.idx` |
| `ComboDictBudget` | 2 KB | diccionario de combinaciones antes de agrupar por prefijo |
| Profundidad | 3 palabras | |
| `CompactSpacing` | 1 s | 7.6 |
| `LeaseTimeout` | 2 min | lease vencido |
| `CompactBytes` | 4 KB | sin cambios |
| `MinIndexDocs` | 100 | sin cambios |
| `PrefixBudget` | 32 KB | 6.3 |

Todos son configurables. Cambiarlos solo afecta a lo que se escribe desde ese momento: el lector no los necesita, porque el `.idx` dice qué está expandido.

---

## 12. Implementación y medición

1. `blob`: `GetRange` devuelve también el ETag del objeto (S3 lo manda en cada rango).
2. `plog`: base de 176 B (7).
3. Paquetes nuevos:
   - `postings` (2.1, 2.2);
   - `bucket` (formatos `.idx` / `.db` y la construcción de un bucket a partir del índice en memoria);
   - `tokens`.
4. `collection`: compactación incremental, lease, reconstrucción.
5. `query`: el lector de 6, con la caché de 10.
6. Se borran `index`, `manifest` y `snapshot`.
7. **Tests:**
   - ida y vuelta de cada formato;
   - **el mismo top que el ranking de referencia** (fuerza bruta sobre todos los documentos), con umbrales chicos para que haya expansiones, triples y cabezas en todas las consultas;
   - compactaciones concurrentes con consultas;
   - caídas a mitad de compactación.
8. **Medición** sobre `productos.csv` (10k) y ×10: GET, rondas y bytes por consulta en frío y en caliente, de 1, 2 y 3 palabras, y el costo de la compactación.
