import { expect, test, type APIRequestContext } from '@playwright/test'

const zipBase64 =
  'UEsDBBQAAAAIAA+ETVzTNyprPwAAAEQAAAAHABwAMDAxLnBuZ1VUCQADHVKPaR1Sj2l1eAsAAQToAwAABOgDAADrDPBz5+WS4mJgYOD19HAJAtKMIMzBAiS3yvAwASluTxfHkIpbyX/+yzMwMzMxvLtafhIozODp6ueyzimhCQBQSwMEFAAAAAgAD4RNXKnVyORAAAAARAAAAAcAHAAwMDIucG5nVVQJAAMdUo9pHVKPaXV4CwABBOgDAAAE6AMAAOsM8HPn5ZLiYmBg4PX0cAkC0kxAzMjBAiT/1EzJAVLcni6OIRW3kv+cP8DAyszYsPJ9ciFQmMHT1c9lnVNCEwBQSwECHgMUAAAACAAPhE1c0zcqaz8AAABEAAAABwAYAAAAAAAAAAAApIEAAAAAMDAxLnBuZ1VUBQADHVKPaXV4CwABBOgDAAAE6AMAAFBLAQIeAxQAAAAIAA+ETVyp1cjkQAAAAEQAAAAHABgAAAAAAAAAAACkgYAAAAAwMDIucG5nVVQFAAMdUo9pdXgLAAEE6AMAAAToAwAAUEsFBgAAAAACAAIAmgAAAAEBAAAAAA=='

async function createAndFinalizeAlbum(
  request: APIRequestContext,
  filename = 'album.zip',
): Promise<string> {
  const zipBuffer = Buffer.from(zipBase64, 'base64')

  const createRes = await request.post('/api/albums', {
    data: {
      filename,
      sizeBytes: zipBuffer.length,
    },
  })
  expect(createRes.ok()).toBeTruthy()

  const created = (await createRes.json()) as {
    albumId: string
    upload: { strategy: string }
  }

  expect(created.upload.strategy).toBe('s3_multipart')

  const initiateRes = await request.post(`/api/albums/${created.albumId}/multipart/initiate`, {
    data: {
      sizeBytes: zipBuffer.length,
      contentType: 'application/zip',
    },
  })
  expect(initiateRes.ok()).toBeTruthy()
  const initiated = (await initiateRes.json()) as {
    uploadId: string
    partSizeBytes: number
    partCount: number
  }

  for (let partNumber = 1; partNumber <= initiated.partCount; partNumber++) {
    const partURLRes = await request.post(`/api/albums/${created.albumId}/multipart/part-url`, {
      data: {
        uploadId: initiated.uploadId,
        partNumber,
      },
    })
    expect(partURLRes.ok()).toBeTruthy()
    const partURL = (await partURLRes.json()) as {
      url: string
      headers: Record<string, string>
    }

    const offset = (partNumber - 1) * initiated.partSizeBytes
    const partPayload = zipBuffer.subarray(offset, Math.min(offset + initiated.partSizeBytes, zipBuffer.length))
    const uploadRes = await request.fetch(partURL.url, {
      method: 'PUT',
      headers: partURL.headers,
      data: partPayload,
    })
    expect(uploadRes.ok()).toBeTruthy()
  }

  const completeRes = await request.post(`/api/albums/${created.albumId}/multipart/complete`, {
    data: {
      uploadId: initiated.uploadId,
      parts: [],
    },
  })
  expect(completeRes.ok()).toBeTruthy()

  const finalizeRes = await request.post(`/api/albums/${created.albumId}/finalize`)
  expect(finalizeRes.ok()).toBeTruthy()

  return created.albumId
}

test('api validation returns INVALID_REQUEST for negative indexes and trailing json body', async ({ page }) => {
  const albumId = await createAndFinalizeAlbum(page.request)

  const imageRes = await page.request.get(`/api/image/${albumId}/-1`)
  expect(imageRes.status()).toBe(400)
  const imageErr = (await imageRes.json()) as { error?: { code?: string; message?: string } }
  expect(imageErr.error?.code).toBe('INVALID_REQUEST')

  const recommendationRes = await page.request.get(`/api/recommendations/${albumId}/-1?limit=12`)
  expect(recommendationRes.status()).toBe(400)
  const recommendationErr = (await recommendationRes.json()) as {
    error?: { code?: string; message?: string }
  }
  expect(recommendationErr.error?.code).toBe('INVALID_REQUEST')

  const trailingBodyRes = await page.request.post('/api/albums', {
    headers: { 'Content-Type': 'application/json' },
    data: '{"filename":"album.zip","sizeBytes":1}{"extra":true}',
  })
  expect(trailingBodyRes.status()).toBe(400)
  const trailingBodyErr = (await trailingBodyRes.json()) as {
    error?: { code?: string; message?: string }
  }
  expect(trailingBodyErr.error?.code).toBe('INVALID_REQUEST')
})

test('recommendation api contract returns not found and valid status envelope', async ({ page }) => {
  const missingRes = await page.request.get('/api/recommendations/non-existent-album/0?limit=12')
  expect(missingRes.status()).toBe(404)
  const missingErr = (await missingRes.json()) as { error?: { code?: string; message?: string } }
  expect(missingErr.error?.code).toBe('NOT_FOUND')

  const albumId = await createAndFinalizeAlbum(page.request)
  const recommendationsRes = await page.request.get(`/api/recommendations/${albumId}/0?limit=12`)
  expect(recommendationsRes.ok()).toBeTruthy()

  const payload = (await recommendationsRes.json()) as {
    status: string
    items: unknown[] | null
  }
  expect(['pending', 'ready', 'partial', 'failed']).toContain(payload.status)
  expect(payload.items === null || Array.isArray(payload.items)).toBeTruthy()
})

test('album search api supports prefix match and returns status metadata', async ({ page }) => {
  const prefix = `search-target-${Date.now()}`
  const albumId = await createAndFinalizeAlbum(page.request, `${prefix}-album.zip`)

  const searchRes = await page.request.get(
    `/api/albums/search?q=${encodeURIComponent(prefix)}&limit=10`,
  )
  expect(searchRes.ok()).toBeTruthy()

  const payload = (await searchRes.json()) as {
    albums?: Array<{
      albumId: string
      originalFilename: string
      indexStatus: string
      indexedCount: number
      failedCount: number
      totalCount: number
    }>
  }

  expect(Array.isArray(payload.albums)).toBeTruthy()
  const matched = payload.albums?.find((item) => item.albumId === albumId)
  expect(matched).toBeTruthy()
  expect(matched?.originalFilename).toContain(prefix)
  expect(['pending', 'ready', 'partial', 'failed']).toContain(matched?.indexStatus ?? '')
  expect(typeof matched?.indexedCount).toBe('number')
  expect(typeof matched?.failedCount).toBe('number')
  expect(typeof matched?.totalCount).toBe('number')
})

test('album search api validates limit', async ({ page }) => {
  const res = await page.request.get('/api/albums/search?limit=0')
  expect(res.status()).toBe(400)
  const body = (await res.json()) as { error?: { code?: string } }
  expect(body.error?.code).toBe('INVALID_REQUEST')
})
