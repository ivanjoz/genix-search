# Plan: índice de búsqueda con palabras de 5 bits, leído por rangos desde S3 Express

> **Desde 2026-10-03 las secciones 5 a 8 (formatos, consulta, escritura) están reemplazadas por `PLAN_BUCKETS.md`**, que es lo que implementa `genix-s3` hoy. Siguen vigentes las secciones 2 a 4 (letras de 5 bits, claves, números, normalizador). Las mediciones de la sección 13 son del diseño anterior.
> Estado: **implementado en `genix-s3/`** (2026-10-02), con el diseño A de D‑5B11 (postings por término). Los resultados medidos de la implementación están en la sección 13 y reemplazan a las estimaciones de Python donde difieren. Reemplaza a los planes anteriores (`plan.md`, shards con colbin, y `PLAN_S3_EXPRESS.md`, sobre el servidor en Rust), que se borraron.
> Objetivo: el backend recibe `polo spiderman rojo` y devuelve los IDs que más se acercan, **sin saber nada del corpus**: todo lo que necesita se calcula a partir de la consulta con tablas fijas, y el índice se lee por rangos de bytes.
> **Qué se optimiza:** primero la **cantidad de requests a S3 por consulta**, después los **bytes leídos por consulta**. El tamaño de los archivos no es el objetivo: un índice 3× más grande vale la pena si baja las lecturas. **Pero la escritura tiene que seguir siendo eficiente**: se descartan diseños que multiplican los objetos o los PUT por escritura (por ejemplo, un objeto por palabra con append en cada cambio). El proyecto **solo devuelve IDs ranqueados**: se guardan IDs y claves de 5 bits (las primeras 8 letras de cada palabra), nunca texto. No se supone nada sobre el tamaño de la colección (`productos.csv` es solo un corpus de prueba) ni sobre los IDs del cliente (`uint32` arbitrarios).
> **Formatos:** los archivos del índice (`k`, `p`), `p.log` y el snapshot son **solo bytes**, con layouts definidos acá. colbin se usa únicamente como formato de comunicación (request/response, en lugar de JSON) y para la metadata (manifest).
> Mediciones: `python3 scripts/p5sim/search.py` (claves, números, slots), `python3 scripts/p5sim/postings.py` (tamaños de `k` y `p`) y `python3 scripts/p5sim/reads.py` (**requests y bytes leídos por consulta**, 3 000 consultas, IDs `uint32` aleatorios) sobre `productos.csv` (10 010 documentos), con el normalizador v3. "IDs mezclados" son 0..N−1 en orden aleatorio; solo se usan para comparar codificaciones entre sí. Los tamaños de postings con colbin se midieron codificando las listas reales con colbin en Go. Lo marcado como *estimado* es cálculo, no codificación real.

## 0. Resumen

**Una palabra son sus primeras 8 letras, a 5 bits cada una** (40 bits como máximo). Con eso:

- **Las dos primeras letras son el slot** (10 bits). El espacio es fijo: 1 024 slots, iguales para cualquier corpus. Una consulta se convierte en un conjunto fijo de slots, y un directorio de ancho fijo al principio del archivo da el rango de bytes de cada slot por aritmética.
- **Dentro del slot, cada clave guarda solo lo que cambia respecto de la anterior**, en unidades de 5 bits. Una palabra corta ocupa menos que una larga. Medido: **19,0 bits por clave contra 40 de una clave fija**.
- **Las claves están ordenadas**: un prefijo es un tramo contiguo del slot y sus postings son un solo rango de bytes.
- **Cortar en 8 letras funciona como un stemming gratis** (`congelada/os` → `congelad`). Para los pocos casos donde junta palabras distintas (`chocolate`/`chocolatera`), cada posting lleva **2 bits con un hash del resto de la palabra**, que le dan más puntaje a la palabra exacta.
- **Los números no ocupan letras.** Son `NUM_1`, `NUM_2` o `NUM_3` seguido de 1, 2 o 3 unidades con el valor en binario (`uint5`, `uint10`, `uint15`; más grande desborda). Quedan ordenados por valor y caben las 26 letras sin pérdida.
- **Los postings son bits sin cabecera.** La cantidad, la codificación, el largo y el primer ID de cada lista están en el diccionario cacheado, así que una lista en `p` no lleva ningún byte de estructura. El primer ID va en el diccionario con **ancho fijo por slot**. Las diferencias entre IDs van con **un ancho fijo por lista o en varint de nibbles**, lo que ocupe menos en cada lista. Las listas de un slot van pegadas en bits. El 42 % de las palabras (las de un solo documento) se resuelve desde el diccionario, sin leer `p`.

**Lecturas por consulta (implementación en Go, 13.2):** con el diccionario en caché, **1 ronda y 3,3–3,6 requests** contando `p.log`. Los bytes leídos dependen de cómo son los IDs del cliente: con IDs densos (autoincremento) son **1,0 KB** a 10k y **7,2 KB** a 100k; con IDs `uint32` aleatorios, 2,4 KB y 20,4 KB. En frío desde cero son 4 rondas. Bajar los requests es lo que queda por decidir (D‑5B10, D‑5B11). Una colección con menos de `MinIndexDocs` documentos (100 por defecto) no tiene índice y se busca con **1 request** (5.8).

---

## 1. Qué necesita el backend para buscar

| Necesidad | Cómo se resuelve sin conocer el corpus |
|---|---|
| Saber dónde leer | Slot = dos primeras letras (10 bits). Directorio de 1 025 entradas en una posición fija del archivo |
| Encontrar la palabra exacta | Recorrer las claves del slot (mediana: 6; máximo a 10k: 280) |
| Prefijo (lo que el usuario está escribiendo) | Tramo contiguo de claves del slot → un rango contiguo de postings |
| Typos | Edit distance sobre las unidades de 5 bits de las claves del slot, y slots vecinos calculados (7.4) |
| Ranking por cobertura y combinaciones | Postings `(ID, posición, cola)` de cada término; adyacencia por posición |
| Rareza (IDF) | `Count` de cada clave, en el diccionario |

Lo único "aprendido" que lee el backend es el propio índice. No hay diccionario de compresión, ni tabla de dígrafos, ni nada entrenado sobre el catálogo.

---

## 2. El alfabeto de 5 bits

### 2.1 Tabla recomendada: 26 letras, sin pérdida

```
0       END   fin de palabra; dentro de una clave, la marca de una continuación (PLAN_BUCKETS.md 16)
1..26   a..z  (después del normalizador: sin acentos, ñ → n)
27      NUM_1 le sigue 1 unidad:  número uint5  (sección 4)
28      NUM_2 le siguen 2 unidades: número uint10
29      NUM_3 le siguen 3 unidades: número uint15
30..31  configurables por el usuario de la librería (2.3)
```

Son 22 + 10 si los dígitos van dentro de las palabras. Pero conviene que no vayan:

- **El normalizador separa letras de dígitos** (`50ml` → `50` `ml`, `q10` → `q` `10`). Son 478 de 72 954 tokens (0,65 %). Así `500ml` y `500 ml` encuentran lo mismo, y la posición sigue diciendo que eran adyacentes.
- Con los dígitos fuera, las 32 posiciones alcanzan para las 26 letras **sin plegar ninguna**. Un token es de letras o es un número, nunca las dos cosas.
- Si un dígito se guarda como letra, `100` < `20` (orden de texto). Con `NUM_n` se ordena por valor.

### 2.2 Variante de 22 letras: plegado fonético solo para el slot

Si se quieren 22 letras como tolerancia a errores (`vaca`/`baca`, `zapato`/`sapato`), **no hay que plegar la clave, solo el slot**: `slot = plegar(primeras 2 letras)`, con `v→b`, `z→s`, `w→u`, `k→c`. No está implementado (D‑5B1). La clave conserva sus letras: `vaca` y `baca` siguen siendo distintas, pero caen en el mismo slot y el fuzzy las ve juntas. El slot sigue siendo una cuenta: `fold[u1]*32 + fold[u2]`.

### 2.3 Códigos 30 y 31: los fija el usuario de la librería

Se configuran en Go al crear el indexador, y cada uno es una **letra extra**: un carácter que el normalizador deja de plegar o de tratar como separador, y que pasa a ser una unidad más dentro de las palabras.

```go
ix, err := genix.NewIndexer(genix.Config{
    Extra: [2]rune{'ñ', 0}, // código 30 = ñ; 31 sin usar
})
```

- **Ejemplos:** `ñ` deja de ser `n` (hoy `piña`/`pinna` y `campaña`/`campana` colisionan: 4 casos en 874 productos con `ñ`), o un símbolo propio del catálogo.
- **Validación:** a lo sumo 2 caracteres, distintos entre sí, que no sean `a–z` ni dígitos (esos ya tienen código).
- **Orden y slots:** los códigos 30 y 31 se ordenan después de la `z`. Los prefijos siguen siendo tramos contiguos y el slot sigue siendo `u1*32 + u2`.
- **Es parte del algoritmo "antes de las claves"** (5.4): cambiar la configuración de una colección existente obliga a reprocesar desde el sistema origen.
- **El backend no confía en lo que le pasen:** la configuración se guarda en el manifest. Todos los archivos llevan `NormID` = CRC32C(versión del normalizador ‖ configuración de los códigos 30 y 31), y el reader rechaza un archivo con otro `NormID`. Así el writer, la compactación y la consulta usan siempre la misma tabla.

---

## 3. La clave de palabra: 8 letras, ancho variable

### 3.1 El corte en 8 letras

| | Tokens | Palabras distintas |
|---|---:|---:|
| **≤ 8 letras (entran enteras)** | **86,43 %** | **77,58 %** |
| ≤ 10 letras | 96,93 % | 93,58 % |
| ≤ 12 letras | 99,31 % | 98,22 % |

7 136 palabras distintas dan **6 753 claves**: 302 claves juntan 685 palabras.

- **240 de las 302 son la misma palabra con otra terminación** (`congelada/os/as`, `original/es`, `infantil/es`, `desodorante/s`). Para buscar es lo deseable: un stemming que no cuesta nada y que se aplica igual a la consulta.
- **62 juntan palabras de distinta raíz**: `chocolate/chocolatera/chocolatina`, `edulcorado/edulcorante`, `tradicion/tradicional`, `almendra/almendrado`, `refrigerado/refrigerante`. Para eso está la cola de 2 bits (3.4).
- Typos después de la letra 8 (`desodorantte`) no cambian la clave: tolerancia gratis.

### 3.2 Formato de las claves de un slot

Las dos primeras letras las da el slot, así que una clave guarda **0 a 6 letras más**. Las claves del slot van ordenadas, y cada una dice cuántas de esas letras comparte con la anterior:

```
entrada := [cabecera: 1 unidad] [letras nuevas: L − s unidades]

  s = letras compartidas con la clave anterior (después del slot)
  L = letras totales después del slot (0..6)
```

**La cabecera entra en una sola unidad de 5 bits**: los pares válidos `0 ≤ s ≤ L ≤ 6` son 28 (y quedan 4 códigos libres). El código es `L·(L+1)/2 + s` (`L = 0` → 0; `L = 1` → 1–2; `L = 2` → 3–5; … `L = 6` → 21–27), y se decodifica con una tabla de 32 entradas. Así todo el stream de claves está en la grilla de 5 bits y se puede decodificar con el kernel de `packed5` (8 unidades = 5 bytes). La grilla de 5 bits es solo para las claves y los números. Los campos del diccionario y los postings tienen otros anchos (5.1, 5.2) y se leen con un lector de bits común, que también sirve para las claves: el kernel es una optimización, no un requisito del formato.

Ejemplo, slot `po`:

```
pollo    s=0 L=3  → [0,3] l l o         4 unidades
polo     s=1 L=2  → [1,2] o             2 unidades
polvo    s=1 L=3  → [1,3] v o           3 unidades
postre   s=0 L=4  → [0,4] s t r e       5 unidades
```

| Formato de clave (8 letras máx.) | 10k docs | bits/clave |
|---|---:|---:|
| Fija, 40 bits | 33,0 KB | 40,0 |
| Fija, 30 bits (el slot ya da 2 letras) | 24,7 KB | 30,0 |
| **Prefijo compartido en el slot, cabecera de 1 unidad** | **15,4 KB** | **18,6** |
| Igual, con una clave completa cada 16 | 15,7 KB | 19,0 |

- **Por qué no fija:** lo único que da el ancho fijo es acceso directo a la clave i (`base + 5·i`). Acá el slot ya es la unidad de lectura (mediana 6 claves) y se recorre en microsegundos. Además, con el diccionario en caché, las consultas no leen claves de S3.
- **Reinicio cada 16 claves:** una clave completa (`s = 0`) cada 16 permite bisecar entre esos puntos cuando un slot crece (a mayor escala). Cuesta 0,4 bits por clave.

### 3.3 Prefijo y comparación

- **Prefijo `pol`:** slot `po` y después el tramo de claves cuya primera letra nueva es `l`. Es contiguo porque las claves están ordenadas, y sus postings también son un rango contiguo (5.2). Un prefijo de más de 8 letras se corta en 8 y pasa a ser una búsqueda exacta.
- **Comparar** es comparar unidades de 5 bits. El edit distance trabaja sobre las unidades, sin crear strings.

### 3.4 La cola de 2 bits (los 2 bits que sobraban del ID)

Cada posting guarda `cola`: `0` si la palabra tiene ≤ 8 letras y, si no, `1 + CRC32C(letras 9 en adelante) mod 3`. Es una función fija, así que el backend la calcula sobre la consulta sin saber nada más.

- La consulta `chocolate` calcula la misma cola. Los postings de la clave `chocolat` con la misma cola reciben un bonus de palabra exacta, y los demás (`chocolatera`, `chocolatina`) quedan detrás, **sin desaparecer**.
- Medido: de los 179 pares de palabras de distinta raíz que comparten clave, **el 72 % se distingue** con los 2 bits.
- Una lista sin palabras largas no guarda colas (bit `Tail = 0` en el diccionario, 5.1): solo pagan las claves que las necesitan.

---

## 4. Números

Hay 10 871 tokens numéricos en 10k documentos y 90 tienen cero inicial.

### 4.1 `NUM_1`, `NUM_2`, `NUM_3`: el número es un binario de 5, 10 o 15 bits

```
NUM_n (1 unidad) + n unidades = el valor en binario, el bit más significativo primero

  NUM_1 + 1 unidad    uint5     0 .. 31
  NUM_2 + 2 unidades  uint10    32 .. 1 023
  NUM_3 + 3 unidades  uint15    1 024 .. 32 767
```

- **El número es su valor, no su texto:** `007` → 7, `0,5` → `0` y `5` (el normalizador separa en la coma). El backend aplica lo mismo a la consulta, así que se encuentra igual.
- **Canónico:** se usa el `n` más chico en que entra el valor. Así `NUM_1 < NUM_2 < NUM_3` también es orden de magnitud, y las claves numéricas quedan **ordenadas por valor**.
- **Overflow (> 32 767):** se guardan los 15 bits bajos (`v mod 32768`). Es lossy a propósito: un número grande colisiona con otros 1 de cada 32 768 valores, y la consulta pasa por la misma cuenta, así que lo encuentra igual. Saturar en 32 767 juntaría todos los números grandes (códigos de barra, por ejemplo) en una sola clave muy frecuente. En el catálogo hay **1** token que desborda (`162641020`).
- **Mismo formato de slot que las palabras:** la clave es `NUM_n` + sus `n` unidades, así que el slot son las dos primeras unidades (`NUM_n` y la unidad alta del valor) y la clave guarda 0 a 2 unidades más, con la misma cabecera `(s, L)` de 3.2.
- Medido: `NUM_1` 44,3 % de los tokens, `NUM_2` 55,1 %, `NUM_3` 0,6 %. Son 363 claves distintas en 67 slots, y 27 860 unidades en total contando el código `NUM_n`.
- **Prefijo numérico** (la última palabra de la consulta es `50` y el usuario sigue escribiendo): los números que empiezan con `50` en decimal son unos pocos rangos de valor (`50`, `500–509`, `5000–5099`, …), y cada uno es un tramo contiguo de claves.

### 4.2 Por qué no varint

| Codificación de los 10 871 números | Unidades (con el marcador) | |
|---|---:|---|
| Decimal, 1 unidad por dígito | 33 284 | |
| Varint: `NUM` + unidades de 4 bits de valor + 1 de "sigue" | 30 548 | −8,2 % |
| **`NUM_n` + n unidades binarias** (4.1) | **27 860** | −16,3 % |

- El varint se delimita solo, pero **no conserva el orden**: con nibbles big‑endian, 255 (`1·F`, `0·F`) se ordena después de 256 (`1·1`, `1·0`, `0·0`) porque el bit de "sigue" pesa más que el nibble.
- Con `NUM_n`, el largo lo dice el código: los 5 bits de cada unidad quedan para el valor y se conserva el orden.

---

## 5. Formatos de archivo

### 5.0 Reglas comunes

- **Solo bytes.** Cada archivo es un header fijo y secciones cuyo offset se calcula por aritmética o sale de una tabla de ancho fijo. No hay mensajes autodescriptos ni tags por campo, y los largos y offsets nunca son varint. El único varint es el `Codec = 1` de las diferencias de IDs (5.2), cuyo largo total está en el diccionario.
- Los IDs son enteros no negativos de hasta 32 bits (`uint32`).
- Enteros **little‑endian**, salvo los streams de 5 bits (LSB‑first, como `packed5`).
- Todo header empieza con 4 bytes mágicos y un `Format uint16`. El reader rechaza un `Format` desconocido: cambiar un layout es subir `Format` y reconstruir.
- Integridad con CRC32C (`crc32.Castagnoli`).
- **colbin solo** en el manifest (metadata) y en el protocolo de consulta y escritura.

```
genix/<col>/<part>/k.<gen>          diccionario        bytes, se cachea entero
genix/<col>/<part>/p.<gen>          postings           bytes, se lee por rangos
genix/<col>/<part>/p.log            log de cambios     bytes, append
genix/<col>/<part>/snap/<gen>.snap  snapshot           bytes
genix/<col>/<part>/m/<gen>.cbm      manifest           colbin (metadata)
```

`<part>` es la partición de la colección (la empresa, en un ORM multi‑tenant): cada una es un índice independiente, con su `p.log` y sus generaciones.

### 5.1 `k.<gen>`: el diccionario

```
offset 0        Header     64 B
offset 64       SlotDir    1 025 × { KOff uint32, POff uint32 }          8 200 B
offset 8 264    Slots      un SlotBlock por slot no vacío, en orden de slot
al final        PageCRC    ⌈|p| / 4096⌉ × uint32                         CRC de cada página de p
```

**Header (64 B):**

```
0   magic "GXK1"        4
4   Format              uint16
6   reservado           uint16 (0)
8   Gen                 uint32
12  Keys                uint32     claves en total
16  Docs                uint32
20  PSize               uint64     tamaño de p.<gen>
28  PageCRCOff          uint32     offset de PageCRC en k
32  DirCRC              uint32     CRC32C del SlotDir (cada SlotBlock lleva el suyo)
36  NormID              uint32     normalizador + códigos 30/31 (2.3)
40  reservado           20 B (ceros)
60  HeaderCRC           uint32
```

**SlotDir:** `KOff[s]..KOff[s+1]` es el rango de bytes del `SlotBlock` de `s` dentro de `k` (vacío si son iguales). `POff[s]..POff[s+1]` es el rango de sus postings en `p`. La entrada de un slot está en `64 + 8·s`: el backend la ubica sin leer nada antes.

**SlotBlock** (alineado a byte):

```
n          uint32          claves del slot
wCount     uint8           bits de Count
wFirst     uint8           bits de FirstID − base
wX         uint8           bits de X (≥ 4)
reservado  uint8           0
base       uint32          menor FirstID del slot
restarts   uint32 × ⌊(n−1)/16⌋   offset en unidades de las claves 16, 32, …
keys       stream de 5 bits (3.2), relleno con ceros hasta el byte
fields     n × { Count: wCount | Tail: 1 | Codec: 1 | X: wX | FirstID − base: wFirst },
           LSB‑first, relleno hasta el byte
crc        uint32          CRC32C de todo lo anterior del bloque
```

- `n` y `restarts` son `uint32` y no `uint16`: no se supone el tamaño de un slot. Cuesta 2 B por slot y por cada 16 claves.
- **Cada `SlotBlock` lleva su CRC.** En frío solo se leen los bloques de los slots de la consulta, así que cada uno se verifica solo. `DirCRC` verifica el `SlotDir`, que se lee en la primera ronda.

- `fields` tiene ancho fijo dentro del slot: los campos de la clave j están en el bit `j·(wCount + 2 + wX + wFirst)`.
- **El primer ID tiene ancho fijo por slot** (`FirstID − base` a `wFirst` bits). Un varint lo haría apenas más chico con IDs mezclados (14,4 contra 15,2 bits en promedio), pero rompería las filas de ancho fijo. Además, a escala pierde: con IDs de 20 bits, un varint 9+4+4+4 gasta 25.
- **`Codec`** dice cómo están las diferencias de esa lista en `p` (5.2), y `X` depende de él:
  - `Codec = 0`, ancho fijo: `X = W`, el ancho de la diferencia más grande de la lista.
  - `Codec = 1`, varint de nibbles: `X = G`, los nibbles que ocupan las diferencias.
- **`Count = 1`:** no hay diferencias que guardar, así que `Tail`, `Codec` y `X` (≥ 6 bits juntos) llevan la posición (4) y la cola (2). **La lista no ocupa nada en `p`**: la palabra se resuelve desde el diccionario cacheado.
- **`Count > 1`:** el largo de la lista en `p` se calcula, no se guarda. Las listas de un slot van pegadas, en bits (5.2):

```
gapBits(j) = Codec = 0 ? (Count−1)·W : 4·G
bits(j)    = Count = 1 ? 0 : gapBits(j) + 4·Count + (Tail ? 2·Count : 0)
bitOff(j)  = 8·POff[s] + Σ bits(i), i < j  dentro del slot
bytes(j)   = [⌊bitOff(j) / 8⌋, ⌈(bitOff(j) + bits(j)) / 8⌉)
```

  Como el slot se lee entero, la suma es gratis y se hace una vez al cachear.

| `k.<gen>` a 10k (IDs mezclados) | Tamaño |
|---|---:|
| Header + `SlotDir` | 8,3 KB |
| Claves (5 bits, prefijo compartido, reinicios) | 16,2 KB |
| Campos (`Count`, `Tail`, `Codec`, `X`, `FirstID`) y headers de slot | ~30,5 KB |
| `PageCRC` (~30 páginas) | 0,1 KB |
| **Total** | **~55 KB** |

### 5.2 `p.<gen>`: los postings

`p` no tiene header: es la concatenación de las listas con `Count > 1`, en orden de slot y de clave. **Dentro de un slot las listas van pegadas en bits**, y solo el principio de cada slot está alineado a byte (`POff[s]`). Cada lista es:

```
gaps   Count−1 diferencias      ID[0] = FirstID (del diccionario); ID[i] = ID[i−1] + gap[i]
         Codec = 0: W bits cada una
         Codec = 1: varint de nibbles, 3 bits de valor + 1 de "sigue" (0–7 en 1 nibble, 8–63 en 2, …)
pos    Count × 4 bits            posición del token en el documento, saturada en 15
tail   Count × 2 bits            solo si Tail = 1
```

- Una entrada por aparición, ordenadas por `(ID, pos)`: la misma palabra dos veces en un documento da `gap = 0`.
- **El builder elige el `Codec` que ocupe menos en cada lista.** El ancho fijo gana cuando las diferencias son parecidas (IDs sin relación con el texto). Los nibbles ganan cuando hay muchas diferencias chicas y pocas grandes, porque una sola diferencia grande ya no obliga a ensanchar todas. Medido en 6.1.
- **Rangos:** una palabra es `bytes(j)`; un prefijo es un solo rango, desde la primera hasta la última clave del tramo; un slot entero es `[POff[s], POff[s+1])`. El lector salta los `bitOff mod 8` bits del primer byte.
- **Por qué pegadas y no alineadas a byte:** alinear cada lista desperdicia 1,76 KB a 10k (1,6 % de `p`, medido) y no da nada, porque los offsets se calculan igual y el lector de bits arranca en cualquier bit. Lo único que se pierde es leer `p` a ojo con un hexdump: lo cubre `gxs inspect PALABRA`, que decodifica el slot de la palabra.

### 5.3 Integridad de `p` por páginas

`PageCRC[i]` es el CRC32C de los bytes `[4096·i, 4096·(i+1))` de `p`. Con la verificación activada, el lector redondea cada rango a páginas de 4 KB y verifica las páginas que lee. `k` se verifica al cachearlo (`HeaderCRC`, `DirCRC` y el CRC de cada `SlotBlock`).

**Medido (13.2): verificar páginas multiplica los bytes leídos** (de 1,0 KB a 10,9 KB por consulta a 10k con IDs densos; de 7,2 a 18,0 KB a 100k), porque las listas de una consulta son mucho más chicas que una página. Por eso queda **desactivada por defecto** (`Options.VerifyPages`). Lo que se verifica igual en cada lectura: el largo de cada lista en bits tiene que coincidir con el que calcula el diccionario, y el ID no puede desbordar.

### 5.4 `p.log`

Es el log de cambios desde la última compactación, y toda consulta lo lee. Cada op tiene un número de secuencia (`seq`) y es un alta o edición (el ID con sus claves nuevas) o un borrado. La `Base` dice sobre qué generación y hasta qué `seq` está compactado el resto. **No guarda texto**: el writer normaliza al recibir y guarda solo las claves de 5 bits del documento.

**Tokens de un documento** (formato común a `p.log` y al snapshot):

```
por cada token, en orden:
  cabecera  1 unidad:  1..8  = palabra de h letras (8 = puede estar cortada)
                       27..29 = NUM_1..NUM_3: número de 1..3 unidades binarias
  cuerpo    h letras, o las n unidades del número, en unidades de 5 bits
después     END (0): fin de los tokens
al final    una cola de 2 bits (3.4) por cada palabra de 8 letras (h = 8), en orden; 0 si tenía
            exactamente 8. Relleno hasta el byte
```

El `END` es necesario porque el relleno puede ser de 5 bits o más y las colas van después de los tokens. Las colas van para toda palabra de `h = 8`: el decodificador no puede saber cuáles tenían más de 8 letras.

```
p.log := Base Frame*

Base (36 B):
  0   magic "GXL1"       4
  4   Format             uint16
  6   flags              uint16     bit 0 = LogOnly (5.8)
  8   NormID             uint32     normalizador + códigos 30/31 (2.3)
  12  ManifestGen        uint32
  16  LastSeq            uint64
  24  UnixMs             int64
  32  CRC32C             uint32

Frame := tag uint8 | len uint32 | crc32c uint32 | body[len]

  tag 1 Batch    FirstSeq uint64 | n uint16 | n × Op
                 Op := ID uint32 | flags uint8 (bit 0 = borrado) | tokLen uint16 | tokens[tokLen]
                 seq del op i = FirstSeq + i; un borrado lleva tokLen = 0
  tag 2 Compact  TargetSeq uint64 | UnixMs int64     pide compactar hasta TargetSeq
  tag 3 Docs     solo sin índice (5.8)
```

- El overlay de la consulta usa las claves tal como están: no normaliza nada al leer el log.
- Un append cortado se detecta por `len` y `crc`. El lector descarta el último frame incompleto.
- Tamaño (*estimado* con el registro promedio medido en 6.3): un op ocupa ~35 B. En 4 KB entran ~115 cambios en un batch, contra ~84 guardando el texto.
- **El sistema habla en claves de 5 bits.** Lo guardado (log + snapshot) alcanza para todo lo que pasa después de las claves:
  - **cambia la data** (documento nuevo, editado o borrado): el log trae `ID` + claves nuevas, y la compactación quita las claves viejas de ese ID (snapshot) y pone las nuevas;
  - **cambia el algoritmo después de las claves** (layout de `k`/`p`, slots, codificación de postings, ranking): se reconstruye desde el snapshot.
- **Si cambia el algoritmo antes de las claves** (el normalizador o la tabla que convierte texto en 5 bits), hay que reprocesar desde el sistema origen, porque el texto no se guarda.

### 5.5 Snapshot `snap/<gen>.snap`

```
Header (36 B): magic "GXN1" | Format uint16 | reservado uint16 | NormID uint32 | Gen uint32 | LastSeq uint64
               | DocCount uint32 | Chunks uint32 | CRC32C uint32
ChunkDir:      Chunks × { FirstID uint32, Off uint32, Len uint32, CRC32C uint32 }
Chunk:         docs ordenados por ID: ID uint32 | tokLen uint16 | tokens[tokLen]   (formato de 5.4)
```

Chunks de ~1 000 documentos. Es el estado `ID → claves` vigente. La compactación lo necesita para saber qué postings quitar cuando un documento cambia o se borra. La consulta nunca lo lee. El delta del snapshot usa el formato `Batch` de `p.log`.

### 5.6 Manifest (colbin)

Es metadata y va en colbin: generación, `LastSeq`, `NormalizerVersion`, la configuración de los códigos 30 y 31 con su `NormID` (2.3), la cantidad de documentos y de claves, los tamaños de `k`, `p` y el snapshot, y `CreatedUnixMs` (el reclamo de la generación, 8). El protocolo de consulta (`QueryRequest`, `QueryResponse`, `Searcher.Serve`) también es colbin. El de escritura todavía no existe: se escribe con la API de Go.

### 5.7 Partición (a escala)

Un `k`/`p` por generación hace que compactar reescriba todo. Con 10k es trivial (~170 KB). A 1M es del orden de decenas de MB por compactación. Opciones, en orden de complejidad:

1. Partir `p` por primera letra (26 objetos + números), con el `SlotDir` global en `k` y un `POff` relativo a cada objeto. Una compactación reescribe solo las letras tocadas, aunque con ~7 palabras por documento un lote de cambios toca casi todas.
2. Base + delta por partición: la base no se reescribe, y cada compactación reescribe un delta acumulativo con las listas de los IDs cambiados (y esos IDs, para anular sus postings en la base). Cuando el delta pasa una fracción de la base (~10 %), se mezclan. La consulta lee base y delta en la misma ronda. El snapshot, que pesa más que el índice, sigue la misma regla.

### 5.8 Colecciones chicas: sin índice, solo `p.log`

`MinIndexDocs` (**100 por defecto**, configurable en `genix.Config` y en `gxs -min-index-docs`): una colección con menos documentos **no tiene índice**: ni `k`, ni `p`, ni snapshot, ni manifest. Todo está en `p.log`:

```
p.log := Base (flags bit 0 = LogOnly) | Docs | Frame*

Docs (tag 3, solo justo después de una Base LogOnly):
  n uint32 | n × { ID uint32 | tokLen uint16 | tokens[tokLen] }     ordenados por ID
```

- `Docs` es el estado de la colección a `Base.LastSeq`, como lo sería el snapshot. Los frames que siguen son los appends normales.
- **La consulta lee solo `p.log`: 1 ronda y 1 request.** Aplica las mismas reglas a los documentos de `Docs` y a las ops pendientes. El IDF usa los documentos vivos, y los typos se buscan solo para términos que ningún documento tiene.
- Tamaño medido: ~34 B por documento (60 documentos de `productos.csv` = 2,0 KB), así que 100 documentos son ~3,4 KB, más los appends hasta `CompactBytes`.
- **Pasar de un modo a otro lo decide la compactación**, por la cantidad de documentos vivos:
  - con `MinIndexDocs` o más, construye la siguiente generación;
  - si los borrados la bajan del umbral, reescribe `p.log` con `LogOnly` y la generación anterior se borra en la compactación siguiente, porque puede haber consultas leyéndola.
- **Disparo de la compactación:**
  - un append que lleva a la colección a `MinIndexDocs` documentos la pide, para que el índice se construya en cuanto hace falta;
  - el umbral de bytes cuenta solo lo agregado desde la base, no el frame `Docs`.
- `Base.ManifestGen` guarda la última generación aunque la colección esté sin índice, así que volver a indexar reclama un número nuevo.
- Una compactación sin índice no reclama generación. Por eso el rebase exige que `p.log` siga en la misma base (`ManifestGen`, `LastSeq` y `LogOnly`), no solo en la misma generación. Si no, dos compactaciones simultáneas podían perder lo que plegó la primera. Hay un test que lo reproduce.
- `MinIndexDocs = 1` indexa siempre (salvo una colección vacía).

---

## 6. Postings: cuánto ocupan y los IDs

### 6.1 Medido a 10k (7 138 listas, 73 545 postings)

**colbin contra bits sin cabecera** (IDs 0..N−1 en el orden del CSV, que está ordenado por nombre):

| Formato | Tamaño |
|---|---:|
| colbin, un mensaje `[]Posting{ID, Pos, Tail}` por lista | 215,9 KB |
| Bits sin cabecera, primer ID en `p` | 136,1 KB |
| Bits sin cabecera, primer ID en el diccionario | 125,5 KB |

- La diferencia está en las listas chicas: el 42 % tiene un solo posting, y las 4 685 listas de ≤ 3 postings ocupan 54 KB en colbin contra 15 KB en bits. Un mensaje colbin trae 5–7 B de estructura propia, y acá todo lo que esa estructura diría ya está en el diccionario.
- El 99,8 % de las posiciones es < 16 (máximo 29): 4 bits alcanzan.

**Codificación de las diferencias** (`p` con el layout de 5.2: solo las 4 118 listas con `Count > 1`; el primer ID va en `k`):

| Diferencias | IDs en orden de nombre | IDs mezclados (realista) |
|---|---:|---:|
| Ancho fijo por lista | 122,6 KB | 115,2 KB |
| Varint de nibbles | 91,2 KB | 120,0 KB |
| **El menor por lista (`Codec`, 5.1)** | **90,2 KB** | **113,0 KB** |
| El menor por lista, con las listas pegadas en bits (5.2) | 88,7 KB | 111,2 KB |

- **Ninguno de los dos gana siempre.** Con IDs mezclados las diferencias de una lista son parecidas y el bit de "sigue" del varint sobra: el ancho fijo gana y se elige en 4 014 de 4 118 listas. Con IDs en orden de nombre, el 42 % de las diferencias vale 1 y los nibbles ganan, en 1 695 listas. Elegir por lista cuesta 1 bit por clave en `k` (~0,9 KB) y da lo mejor en los dos casos.
- Probados y descartados para las diferencias: varint de 5 bits (4+1), de bytes (7+1) y de 9+5. Todos ocupan más que los nibbles, porque la mayoría de las diferencias necesitan pocos bits y un tramo mínimo grande se paga igual.

### 6.2 IDs: los del cliente, sin suponer nada

Los IDs son los del cliente, `uint32` arbitrarios: no se supone que sean seguidos ni densos, y no hay tabla de traducción a un número interno. Las mediciones de 6.1 con IDs 0..N−1 sirven para comparar codificaciones entre sí. Los bytes leídos por consulta (`scripts/p5sim/reads.py`) se miden con IDs `uint32` aleatorios.

### 6.3 Lecturas por consulta: postings contra documentos en el grupo (medido)

3 000 consultas de 2–3 palabras sacadas de documentos del catálogo, IDs `uint32` aleatorios. Ranking de referencia (exacto): cobertura, después palabras de la consulta adyacentes, después posición, después ID.

**"Requests de datos" no cuenta todo:** supone el diccionario ya en caché (en frío hay que leerlo antes: 2 rondas más) y no cuenta `p.log`, que se lee siempre en la misma ronda. En caliente, B son **2 requests** (`p.log` + el grupo) en 1 ronda; A son 1 + 2,34.

| Diseño del grupo de una palabra | Requests de datos | Bytes leídos (prom. / p50 / p99) | Top‑10 igual al exacto |
|---|---:|---:|---:|
| **A. Postings** (5.2): se lee la lista de cada término | 2,34 | 1 229 / 672 / 5 786 | exacto |
| **B. Claves del documento en el grupo**: cada registro es `ID` + las claves de 5 bits de todas las palabras del documento, sin texto (31,5 B en promedio); se lee solo el grupo del término más raro y el resto de la consulta se evalúa sobre esos documentos | **1,00** (0,82 si los grupos ≤ 64 B van dentro del diccionario) | 988 / 291 / 12 959 | 19,2 % |
| C. Como B, con los dos términos más raros | 2,00 | 6 200 / 2 361 / 42 195 | 78,0 % |

Con la última palabra escrita a medias (prefijo de 3 letras o más):

| | Requests de datos | Bytes (prom. / p99) |
|---|---:|---:|
| A: listas de los términos completos + el rango del prefijo | 2,36 | 1 391 / 5 998 |
| B: grupo del término completo más raro; el prefijo se evalúa sobre los candidatos | **1,00** | 2 903 / 36 141 |

Qué dicen los números:

- **B garantiza los documentos que tienen todas las palabras**, con un solo request: todo documento que las tiene está en el grupo de la más rara, y trae las claves de 5 bits de todas sus palabras para puntuar el resto. No es texto: son las mismas claves de 8 letras del índice, y la respuesta sigue siendo solo IDs. Además, el prefijo y los typos de las otras palabras (incluidos los de las dos primeras letras) se evalúan sobre los candidatos sin leer nada más.
- **B no ve los documentos a los que les falta la palabra más rara.** Solo en el 12,7 % de las consultas hay 10 o más documentos con todas las palabras (22,3 % con 5 o más). En el resto, el top‑10 exacto incluye documentos con cobertura parcial que B no tiene. Por eso coincide en el 19,2 %.
- **A es exacto y lee menos en la cola** (p99 5,8 KB contra 13–36 KB), pero hace un request por término.
- Por pigeonhole, leer los r grupos más raros encuentra todo documento con cobertura ≥ T − r + 1. Con r = T es exacto, pero lee mucho más que A.

La elección depende del contrato del ranking (D‑5B11). Si los documentos con todas las palabras deben ir primero y los parciales solo completan cuando faltan, B resuelve la mayoría de las consultas con 1 request. Si el top‑K debe ser exacto también sobre los parciales, A es mejor.

### 6.4 Leer solo el frente de una lista (a escala)

Para palabras con millones de postings (`ml`, `pack`), una variante de ancho fijo permite leer solo el principio de la lista sabiendo exactamente qué bytes pedir: 5 B por posting, `uint32` (ID de 30 bits + cola de 2) + 1 B de posición, con la lista ordenada por calidad estática. Ocupa unas 3× más (359 KB a 10k para todo el índice). Se usaría solo para las claves con `Count` sobre un umbral, marcadas en el diccionario. No hace falta en v1.

---

## 7. Ruta de consulta

### 7.1 `polo spiderman rojo`

```
1. normalizar      → polo · spiderman · rojo         (normalizador + corte letras/dígitos)
2. claves          → polo · spiderma · rojo          (8 letras máx.; cola de "spiderman" = hash("n"))
3. slots           → po, sp, ro                      (u1*32 + u2)
4. diccionario     → recorrer el SlotBlock de cada slot (en caché)
                     exacto; la última palabra también como prefijo; typos: 7.4
                     Count = 1 → el posting ya está en el diccionario
5. rangos          → bitOff(j), bits(j) → rangos en p.<gen>; se fusionan los cercanos
                     (y se redondean a páginas solo con VerifyPages, 5.3)
6. UNA ronda       → GET p.log + GET de los rangos de p.<gen>, en paralelo
7. puntuar         → cobertura, adyacencia por posición, IDF por Count,
                     bonus si la cola coincide, overlay de p.log
```

### 7.2 Rondas

| Estado | Rondas | Requests |
|---|---|---|
| Sin índice (menos de `MinIndexDocs` documentos, 5.8) | 1 | `p.log` |
| `k` en caché | 1 | `p.log` + 1 por término (menos, si los rangos se fusionan o la palabra tiene `Count = 1`) |
| Generación conocida, slots sin cachear | 2 | `p.log` + `SlotBlock`s; postings |
| En frío desde cero | 4 | `p.log` (dice la generación); manifest + `Header` + `SlotDir`; `SlotBlock`s; postings |
| En frío con `Preload` (todo `k`) | 3 | `p.log`; manifest + `k` entero; postings |

En frío la generación sale de `p.log`, así que antes de leer `k` hay una ronda más que la que contaba la versión anterior de esta tabla. Los typos en frío suman una ronda (los slots vecinos se piden cuando el término no está en el diccionario).

### 7.3 Prefijos cortos

La última palabra se busca también como prefijo. Con 2 letras, el prefijo es el slot entero (`ca`: 280 claves a 10k, ×10 a 100k), y con 1 letra son 27 slots seguidos, que también son un solo rango de `p`. Para que un prefijo corto no lea medio índice:

- se ordenan las claves del tramo por `Count` y se toman las de más documentos hasta un presupuesto de bytes (`Options.PrefixBudget`, 32 KB por defecto), con los largos calculados de 5.1. Las claves con `Count = 1` no cuestan nada (están en el diccionario) y entran siempre;
- **si el término no tiene clave exacta**, la mejor clave del tramo entra aunque pase el presupuesto: el término lee lo mismo que leería una palabra exacta. Sin esto, una palabra frecuente a medio escribir (`carre` a 100k) no encontraba nada;
- con 1 letra (o 1 dígito), el prefijo no se expande: solo cuenta la palabra exacta (`x`, `l`);
- la respuesta marca `Approx` cuando el presupuesto dejó claves afuera.

### 7.4 Typos sin conocer el corpus

- **Typo entre las letras 3 y 8** (`spidreman`): edit distance ≤ 1–2 entre la consulta y las claves del slot `sp`, unidad por unidad. Medido: un slot tiene 6 claves de mediana y 280 como máximo (`ca`).
- **Typo después de la letra 8:** no cambia la clave.
- **El corte en 8 cambia el edit distance:** `chocoate` (falta la `l`) contra la clave `chocolat` da 2, aunque contra `chocolate` sería 1, porque el corte se llevó la `e` final. Cuando la clave tiene 8 letras (puede estar cortada), se usa **edit distance de prefijo**: el mínimo entre la clave y los prefijos de la consulta de `8 − d` a `8 + d` letras, con `d` el edit distance permitido. Por ejemplo, `chocoat` (las primeras 7 letras de `chocoate`) contra `chocolat` da 1. Las claves de menos de 8 letras se comparan normal.
- **Typo en las dos primeras letras** (`psiderman`, `siderman`): los slots candidatos se calculan con la consulta: transposición (`ps` → `sp`), borrado de la primera letra, letra insertada, sustitución de la segunda. Son unas decenas de slots (~80 con edit distance 1). Como el diccionario está en caché, se revisan en memoria y **solo se piden los postings de las claves que pasan el edit distance**. Leer slots vecinos no cuesta requests.
- Con el plegado de 2.2, los errores `b/v`, `s/z` y `c/k` al inicio ni siquiera necesitan esto: caen en el mismo slot.
- Los typos se buscan solo para términos que no están en el diccionario (ni exactos ni como prefijo). El presupuesto de 7.3 también se aplica, ordenando por edit distance y después por `Count`, y la mejor clave entra siempre.

---

## 8. Escritura

Implementado en `internal/collection` y `internal/plog`.

**Append** (`Apply`; `Put` y `Delete` son sus casos de un solo tipo):

1. El writer normaliza el texto y genera las claves y colas con las mismas funciones que el builder. La consulta usa las claves del log tal como están.
2. Lee `p.log` (un GET) y arma un frame `Batch` con `seq` = último + 1, …: altas, ediciones y bajas en el mismo batch, las bajas al final (un ID en las dos queda borrado).
3. Lo agrega con el append nativo de S3 Express (`WriteOffsetBytes` = el tamaño leído). Un batch es un solo append, que es atómico.
4. Si el offset no coincide (otro writer o una compactación tocaron el log), vuelve a leer y reintenta, con un backoff proporcional a lo que tardó el intento perdido (dos writers chocan mientras alguno está entre su lectura y su escritura, que es una ronda al store). Así varios writers quedan serializados sin locks.
5. Si lo agregado desde la base pasa `CompactBytes`, o la colección llega a `MinIndexDocs` (5.8), el mismo append lleva un frame `Compact` y el resultado dice `CompactRequested`. Quien escribe llama a `Compact` (o el `Client` según su modo, abajo).
6. **Creación perezosa:** si `p.log` no existe, el primer alta lo crea con un PUT `If-None-Match`: base `LogOnly` sin documentos más el batch, en 2 requests (el GET 404 y el PUT). Si otro writer lo creó primero, recibe 412 y agrega sobre el suyo. Un batch de solo bajas sobre una colección inexistente no escribe nada.
7. **Pedido perdido:** mientras haya un `Compact` pendiente, los appends no piden otro. Si el proceso que debía compactar murió, nadie lo pediría más; por eso un pedido pendiente más viejo que `StaleCompact` (1 min por defecto) se considera perdido y el siguiente append lo vuelve a pedir.

**Compactación** (`Compact`):

1. Lee `p.log`: la base (generación M) y las ops pendientes hasta `TargetSeq`, el último `seq`. Un log con la cola corrupta no se compacta.
2. Lee el snapshot de M (o el frame `Docs`, sin índice), le aplica las ops y construye `k`, `p` y el snapshot de G = M + 1.
3. **Reclamo:** `PUT m/<G>.cbm` con `If-None-Match`. Va antes que los objetos, así que solo una compactación escribe objetos con G en la clave. Si G es un reclamo abandonado (sin publicar y más viejo que `ClaimTimeout`, 5 min), usa el siguiente número libre.
4. Escribe `k`, `p` y el snapshot.
5. **Commit:** reescribe `p.log` con `If-Match`: la base nueva (G, `TargetSeq`) más las ops y los pedidos de compactación posteriores a `TargetSeq`. Si recibe 412 (un writer agregó algo en el medio), vuelve a leer y reintenta. Si la base ya no es la que leyó en el paso 1, aborta con `ErrConflict` y borra su reclamo y sus objetos.
6. **GC** sin `List`, best effort: borra la generación M − 1. La M sigue viva, porque una consulta que leyó `p.log` antes del commit puede estar leyéndola.

**Por qué no se pierde nada:**

- Los writers solo agregan, con un offset exacto, y la compactación solo reescribe el log con `If-Match`. Ninguno puede pisar al otro sin que el otro reciba un error.
- Las ops que llegan durante la compactación tienen `seq > TargetSeq` y se conservan en el rebase.
- Las consultas leen `p.log` entero, y S3 tiene consistencia fuerte: ven el log viejo con la base M o el nuevo con la base G, nunca una mezcla. Si la generación que la consulta tenía en caché ya no está, la descarta y reintenta.
- Igual, el lector verifica `len` y `crc` de cada frame y descarta un frame final cortado.

**Reconstrucción** (`Rebuild`): reprocesa la colección desde el sistema origen. Es también la forma de cambiar las letras extra o el normalizador (5.4).

1. Lee `p.log` y anota su último `seq`, S0, **antes** de leer la fuente.
2. Reclama la generación siguiente, como una compactación, para que ninguna compactación con índice publique mientras se lee la fuente (la suya choca con el reclamo y devuelve `ErrConflict`).
3. Lee la fuente (`Source`, una función que la librería puede llamar más de una vez) y construye la generación, o un `p.log` `LogOnly` si queda bajo `MinIndexDocs`.
4. Rebase de `p.log` con `If-Match`, conservando las ops posteriores a S0.

Los writers agregan al log **después** de que su escritura en la fuente confirmó. Entonces una op con `seq ≤ S0` ya está en lo que devuelve la fuente, y las posteriores quedan pendientes encima; como son idempotentes, aplicar una que la fuente ya tenía no cambia nada. Si una compactación `LogOnly` (que no reclama nada) publica en el medio, o un writer crea la colección mientras se reconstruía una inexistente, la reconstrucción vuelve a empezar (hasta 4 veces). Un `p.log` de otro normalizador se reemplaza entero: sus ops están en claves que este no lee, y la fuente ya tiene su efecto.

**Vaciado** (`Drop`): una reconstrucción con la fuente vacía, que además borra la generación vigente (una consulta en vuelo la encuentra borrada, relee `p.log` y lo ve vacío). `p.log` queda como un log `LogOnly` sin documentos, así la numeración de generaciones sigue desde donde estaba.

**`Client`** (multi‑partición): un solo cliente para todas las particiones de todas las colecciones de un store. Guarda un LRU de buscadores (`MaxSearchers`, 256 por defecto), uno por partición consultada, cada uno con la caché de slots de su generación (8 MB como máximo cada uno). Una partición inexistente no tiene resultados. La compactación que pide una escritura la corre según `CompactMode`:

| Modo | Quién compacta |
|---|---|
| `CompactSync` | la misma escritura, antes de que `Apply` vuelva (~133 ms a 100k documentos) |
| `CompactAsync` | una goroutine, una a la vez por partición; si llega otro pedido mientras corre, corre una vez más al terminar |
| `CompactCallback` | quien llamó, avisado por `OnCompact` (p. ej. para encolar en SQS) |
| `CompactAuto` (por defecto) | `Sync` en AWS Lambda, que congela las goroutines al responder; `Async` en el resto |

**Verificado en un directory bucket** (`s3blob`, 13.6): el append con `WriteOffsetBytes`, `If-None-Match` e `If-Match` se comportan como supone el diseño, también con escritores concurrentes. La cola SQS FIFO que se había previsto por si faltaba `If-Match` no hace falta.

---

## 9. Alineación de bits

| Estructura | Alineación |
|---|---|
| Stream de claves de un slot | Todo en unidades de 5 bits (cabecera de 1 unidad + letras), kernel de 8 unidades = 5 bytes. Al final, relleno hasta el byte: el decodificador sabe que son `n` claves, así que el relleno no se interpreta |
| `fields` de un slot | Ancho fijo por slot (`wCount + 2 + wX + wFirst` bits), relleno hasta el byte al final |
| Listas de postings de un slot | Pegadas en bits, una detrás de otra. Cada lista: diferencias (W bits cada una, o nibbles de varint), posiciones de 4 bits, colas de 2 bits. El offset de cada una se calcula en bits desde el diccionario; solo el principio del slot está alineado a byte |
| Headers, `SlotDir`, `PageCRC`, `p.log`, snapshot | Enteros de ancho fijo, little‑endian |

El relleno es como mucho 7 bits por bloque alineado: a 10k, < 300 B en `k` y otro tanto en `p`, uno por slot.

---

## 10. Riesgos

| Riesgo | Mitigación |
|---|---|
| Catálogos con muchos números > 32 767 (SKUs, códigos de barra) | Con overflow a 15 bits colisionan entre sí: un SKU buscado trae otros que caen en la misma clave. En el catálogo de prueba es 1 token, pero no se puede suponer para otros. Se mide la tasa de colisión al indexar y se reporta en el manifest |
| El corte en 8 junta palabras distintas (62 claves a 10k) | Cola de 2 bits: el 72 % se distingue en el ranking. Si no alcanza, ampliar la cola a 3–4 bits, que solo pagan las listas con `Tail = 1` |
| Slots desbalanceados (`ca`: 280 claves; postings ×10 a 100k) | Con el diccionario en caché el tamaño del slot no se lee: solo se leen los postings de las claves pedidas. Lo que crece son las listas de palabras frecuentes: lectura del frente (6.4) |
| Slots con muchas claves a escala | Reinicio cada 16 claves (3.2) para bisecar dentro del slot |
| El diccionario crece con el vocabulario | En frío se leen solo los `SlotBlock` de los slots de la consulta, no el diccionario entero. Lo que cuesta es la ronda extra (7.2), no el tamaño |
| Cambiar el algoritmo | Después de las claves (layout, slots, postings, ranking): se reconstruye desde el snapshot. Antes de las claves (normalizador, tabla de 5 bits): reprocesar desde el sistema origen (5.4) |

## 11. Decisiones abiertas

| # | Pregunta | Opciones | Recomendación inicial |
|---|---|---|---|
| D‑5B1 | Letras | 26 sin pérdida / 22 con plegado | 26 en la clave; plegado opcional solo en el slot (2.2) |
| D‑5B2 | Tokens mixtos | conservar `50ml` / separar `50` `ml` | separar |
| D‑5B3 | Cola de palabras largas | 2 bits / 3–4 bits / nada | 2 bits; medir la relevancia con la suite |
| D‑5B5 | Integridad de `p` | CRC por página de 4 KB / sin verificar en la consulta | **sin verificar por defecto** (medido: las páginas multiplican los bytes leídos, 5.3); las páginas quedan como opción |
| D‑5B6 | Objetos | un `k`/`p` por generación / `p` partido por primera letra | uno en v1; partir cuando la compactación pese |
| D‑5B7 | Listas enormes | bits por lista / variante de ancho fijo para leer el frente (6.4) | bits en v1 |
| D‑5B9 | Prefijos cortos (7.3) | presupuesto por `Count` / exigir ≥ 3 letras | presupuesto; 1 letra sin expansión |
| D‑5B10 | Qué guarda el grupo de una palabra (6.3) | postings de esa palabra / los documentos enteros, en claves de 5 bits | pendiente: depende de D‑5B11 |
| D‑5B11 | Contrato del ranking | exacto sobre todos los documentos con alguna palabra / primero los que tienen todas, el resto solo si faltan | pendiente; implementado A (exacto) |

## 12. Plan de trabajo

Hecho: el alfabeto y las claves, el normalizador v4, `k` y `p`, `p.log` y el snapshot, la consulta, la escritura con compactación, el modo sin índice y las mediciones (13). Queda:

1. ~~**Cliente de S3 real.**~~ Hecho: `s3blob`, verificado contra un directory bucket (13.6). Falta medir la latencia por ronda desde la misma AZ.
2. **D‑5B11**, el contrato del ranking, que decide D‑5B10 y si se puede bajar a 1 request de datos (6.3).
3. **Suite de relevancia con juicios humanos.** La comparación de 13.3 es contra referencias que aplican las mismas reglas, no contra lo que un usuario espera. Casos: `spiderman polo` contra `polo spiderman rojo`, `pimienta moli`, `sunrise tequila`, tildes y `ñ`, `carrefour` ≡ `carefour`, `pollo` ≠ `polo`, `sin gluten`, typos (`spidreman`, `camizeta`), que un match con typo nunca supere al exacto, y cuánto cambia el ranking por el corte en 8.
4. ~~**Disparo de la compactación.**~~ Hecho: los modos del `Client` (8). Quedan las Lambdas de consulta y de escritura con protocolo en colbin, si se usan fuera de Go; el ORM usa el `Client` directo.
5. **Fuzzing** de los lectores de `k`, `p.log` y el snapshot.
6. **Partir `p`** (5.7) cuando la compactación pese. No confundir con las particiones de una colección (5.0), que ya existen.
7. **Integración con Genix ORM** (Scylla y DynamoDB): el `Client` es el backend de búsqueda de las tablas que la declaran.

---

## 13. Implementación y mediciones

### 13.1 Código (`genix-s3/`)

El código anterior de `genix-s3` (shards con colbin) y sus planes se borraron. Hay copia en `../genix-s3-legacy-2026-10-02.tar.gz` y `../genix-plans-legacy-2026-10-02.tar.gz`, fuera del repo.

| Paquete | Qué hace |
|---|---|
| `genix` (raíz) | API pública: `Client` (todas las particiones: `Apply`, `Search`, `Compact`, `Rebuild`, `Drop`), y para una sola colección `Config{Partition, Extra, MinIndexDocs}`, `NewIndexer`, `NewSearcher`, `Serve` (request/response en colbin) |
| `internal/bits` | Lector y escritor de bits LSB‑first |
| `internal/blob` | La interfaz del store, `blob.FS` (directorio local) y `blobtest`, el contrato que todo store cumple |
| `s3blob` | El store en S3: append nativo en un directory bucket (`--x-s3`), emulado con GET + PUT `If-Match` en un bucket de propósito general |
| `internal/normalize` | Normalizador v4 (letras/dígitos separados, letras extra 30/31, `NormID`) |
| `internal/key5` | Claves de 5 bits, `NUM_n`, slot, cola, stream de un slot, stream de tokens, edit distance de prefijo |
| `internal/index` | `k` y `p` (5.1–5.3): builder, reader por slot, decodificación de listas, páginas |
| `internal/plog`, `internal/snapshot` | 5.4 y 5.5 en bytes |
| `internal/manifest` | 5.6 en colbin |
| `internal/collection` | Escritura (8): `Init`, `Apply`, `Compact` (claim con If‑None‑Match, rebase de `p.log` con If‑Match, GC de la generación anterior, cambio de modo de 5.8), `Rebuild` y `Drop` |
| `internal/query` | Consulta: plan (exacto, prefijo con presupuesto, typos), rondas, caché de slots, overlay, ranking |
| `cmd/gxs` | CLI: `build`, `query`, `put`, `del`, `compact`, `log`, `inspect PALABRA` (el dump por slot de 5.2) |

Tests (todos con `-race`): bytes dorados de `p` y de un `SlotBlock`, roundtrip y corrupción byte por byte de `k`, el `Codec` elegido siempre es el menor, `p.log` cortado en cada byte, appends concurrentes, compactación con generación cacheada, búsquedas concurrentes con escrituras y compactaciones, **el motor contra una referencia que rankea todos los documentos con las mismas reglas**, con ops pendientes en `p.log`, y el ciclo de vida sin índice → con índice → sin índice (5.8), incluida la carrera entre dos compactaciones sin índice.

### 13.2 Lecturas por consulta

`productos.csv`, 3 000 consultas de 2–3 palabras de nombres del catálogo (la última también como prefijo), top 10. Store local con contador de requests y bytes. Los bytes incluyen `p.log` (36 B recién compactado). x10 repite el catálogo con IDs nuevos.

**IDs densos** (1..N, como un autoincremento; es lo que usaba la evaluación del `genix-s3` anterior):

| | Rondas | Requests | Bytes prom. / p50 / p99 |
|---|---:|---:|---:|
| 10k, `k` en caché | 1 | 3,30 | 1,0 KB / 560 B / 4,5 KB |
| 10k, `genix-s3` anterior (shards colbin) | 1 | 3,5 | 1,1 KB / 650 B / 4,4 KB |
| 100k, `k` en caché | 1 | 3,56 | **7,2 KB** / 4,2 KB / **30,6 KB** |
| 100k, `genix-s3` anterior | 1 | 3,5 | 10,1 KB / 5,7 KB / 42,5 KB |
| 100k, en frío desde cero | 4 | 7,76 | 17,4 KB / 14,5 KB / 40,8 KB |
| 100k, en frío con `Preload` | 3 | 5,56 | 71,9 KB / 68,8 KB / 95,3 KB |

**IDs `uint32` aleatorios** (el caso general):

| | Rondas | Requests | Bytes prom. / p50 / p99 |
|---|---:|---:|---:|
| 10k, `k` en caché | 1 | 3,38 | 2,4 KB / 1,4 KB / 10,2 KB |
| 100k, `k` en caché | 1 | 3,57 | 20,4 KB / 11,3 KB / 90,5 KB |
| 100k, en frío desde cero | 4 | 7,80 | 30,8 KB / 21,7 KB / 99,7 KB |
| 100k, `k` en caché, `p.log` lleno (4,1 KB) | 1 | — | 24,1 KB |

Qué dicen:

- **Requests:** iguales a los del diseño anterior (~3,5: `p.log` + ~2,3 de listas). El formato no los baja, porque sigue siendo un request por término. Bajarlos es D‑5B11.
- **Bytes, con IDs densos:** −29 % en promedio y −28 % en p99 a 100k contra el diseño anterior.
- **Bytes, con IDs aleatorios: ×2,4–2,8.** Una diferencia entre IDs aleatorios de 32 bits necesita ~log₂(2³²/df) bits: medido, **29,8 bits por posting contra 9,4** con IDs densos (`p` de 266 KB contra 89 KB a 10k). Está cerca del mínimo teórico para IDs dispersos, así que solo lo bajaría traducir los IDs a números internos, y eso se descartó (sin `ids.<gen>`). El formato no supone nada: con IDs densos el ancho por lista se achica solo.
- **Las estimaciones de `reads.py` (1 229 B con IDs aleatorios) eran optimistas.** La implementación lee 2,4 KB.
- **Fusionar rangos** (`CoalesceGap` 4 KB): −0,15 requests y +260 B a 10k con IDs densos; a 100k casi no cambia nada, porque las listas de una consulta caen en slots lejanos.
- **Verificar páginas** (5.3): de 1,0 a 10,9 KB a 10k, y de 7,2 a 18,0 KB a 100k. Queda desactivado.
- **`p.log` se lee en cada consulta**: lleno (4,1 KB, 71 appends de un documento) suma ~4 KB por consulta. `Options.CompactBytes` (4 KB por defecto) es la perilla.
- **En frío** el piso son las 4 rondas (`p.log` → manifest y `SlotDir` → slots → listas). Leer `k` entero (`Preload`) ahorra una ronda a cambio de ~60–70 KB.

### 13.3 Ranking

| Comparación (10k, IDs aleatorios) | Resultado |
|---|---|
| Motor contra las mismas reglas aplicadas a todos los documentos | **idéntico en 3 000/3 000** (también a 100k) |
| Contra un ranking por palabras completas (sin corte en 8, posiciones hasta 255) | orden idéntico 89,5 %, mismos IDs 90,2 %, solapamiento del top 10 95,2 % |
| Typos (500 consultas de 2 palabras, una con 1 error) | el top 1 cubre las 2 palabras en 482/500; el documento original queda en el top 10 en 457/500 |

- **Las 315 diferencias contra palabras completas vienen del stemming gratis del corte en 8**: `arandano`/`arandanos` y `antiparasitario`/`antiparasitaria` comparten clave y cuentan como match (con la cola distinta, debajo de la palabra exacta). En las 315, el primer resultado cubre todas las palabras de la consulta. 243 de ellas tienen una palabra de más de 8 letras.
- A 100k la coincidencia sube al 99,2 % porque cada nombre está 10 veces y el top 10 se llena de copias empatadas.

### 13.4 Escritura

| 100k docs, IDs aleatorios | |
|---|---|
| Build inicial | 0,25 s · 5 PUT · 5,95 MB (`k` 76 KB, `p` 2,6 MB, snapshot 3,3 MB) |
| `Put` de 1 documento | 1 GET (`p.log`, 36 B) + 1 append de 65 B |
| `p.log` hasta el umbral de compactación | 72 appends de un documento |
| Compactación de 72 ops | 133 ms · 4 lecturas (3,3 MB, casi todo el snapshot) · 5 escrituras (6,0 MB) |

La compactación reescribe `k`, `p` y el snapshot enteros: el costo crece con la colección, no con el cambio. Es lo que 5.7 propone partir cuando pese.

### 13.5 CPU

`go test -bench` en un i7‑1355U, 10k documentos:

- Búsqueda en caliente: **141 µs** y 106 allocs por consulta (lectura de archivos locales incluida).
- Plan, decodificación y ranking sin I/O: 85 µs.
- Decodificar una lista: 19,6 ns por posting.
- Build: 28 ms.

El ranking elige el top `offset + limit` con un heap en vez de ordenar todos los candidatos, y los matches de cada documento van en slabs. Las dos cosas bajaron la búsqueda de 241 µs y 1 218 allocs a los valores de arriba.

### 13.6 S3 Express real

Directory bucket `genix-test--use1-az4--x-s3` (us-east-1). Los tests corren con `GENIX_S3_TEST_BUCKET=<bucket> go test ./s3blob`; sin la variable se saltan.

- **Contrato** (`blobtest`, el mismo que cumple `blob.FS`): `If-None-Match` y `If-Match` devuelven 412, un `If-Match` sobre una clave inexistente cuenta como precondición fallida, el append con un offset viejo falla, y con 8 escritores a la vez sobre el mismo offset o la misma clave nueva gana exactamente uno. Un rango que empieza justo en el final devuelve vacío, como en el store local; S3 responde 416, y `s3blob` lo resuelve con un `HEAD`.
- **Ciclo de vida con la API pública** (`TestLifecycle`): build de 300 documentos, 40 `Put` desde 4 escritores concurrentes, un `Delete`, compactación (gen 2, 41 ops, 4 lecturas, 5 escrituras, 19 KB) y búsquedas antes y después. En frío: 4 rondas y 5 requests. En caliente: 1 ronda y 2 requests.
- **`Client` multi‑partición** (`TestClient`):
  - Una partición nueva se crea con su primera escritura en 2 requests, y se consulta en 1 (`LogOnly`).
  - Una partición inexistente responde vacío sin requests.
  - Reconstruir 400 documentos con un alta y una baja concurrentes en medio: la escritura creó la partición durante la lectura de la fuente, la reconstrucción volvió a empezar y conservó las dos.
  - `Drop` deja la partición vacía.
- **Contención:** con 4 escritores sobre la misma partición y ~100 ms por request, el backoff con techo fijo de 64 ms agotaba los 16 reintentos (los escritores volvían a chocar, porque cada intento dura una ronda). Con el backoff proporcional a la duración del intento perdido (8) pasó 3 de 3 veces.
- **Latencia desde fuera de AWS** (WAN, no la AZ): ~100 ms por request en GET, rango, append y PUT. Casi todo es el viaje de ida y vuelta; falta medir desde una Lambda en `use1-az4`.
- Un bucket de propósito general no tiene append. `s3blob` lo emula con GET + PUT `If-Match`, que reescribe el objeto entero (`p.log` pesa ≤ 4 KB). Ese camino todavía no se probó contra S3.
