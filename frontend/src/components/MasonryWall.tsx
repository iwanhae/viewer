import { Fragment, type ReactNode } from 'react'
import { distributeMasonry } from '../utils/masonry'

type MasonryItemRenderer<T> = (item: T, index: number) => ReactNode

interface MasonryWallProps<T> {
  items: T[]
  columnCount: number
  getItemWeight: (item: T) => number
  renderItem: MasonryItemRenderer<T>
  containerClassName?: string
  columnClassName?: string
  containerTestId?: string
  columnTestId?: string
  getItemKey?: (item: T, index: number) => string | number
}

export function MasonryWall<T>({
  items,
  columnCount,
  getItemWeight,
  renderItem,
  containerClassName,
  columnClassName,
  containerTestId,
  columnTestId,
  getItemKey,
}: MasonryWallProps<T>): ReactNode {
  const columns = distributeMasonry(items, columnCount, getItemWeight)

  return (
    <div className={`wall-grid ${containerClassName ?? ''}`.trim()} data-testid={containerTestId}>
      {columns.map((column, columnIndex) => (
        <div
          className={`masonry-column ${columnClassName ?? ''}`.trim()}
          data-testid={columnTestId}
          key={columnIndex}
        >
          {column.map((item, itemIndex) => {
            const key =
              getItemKey !== undefined ? getItemKey(item, itemIndex) : `${columnIndex}-${itemIndex}`
            return <Fragment key={key}>{renderItem(item, itemIndex)}</Fragment>
          })}
        </div>
      ))}
    </div>
  )
}
