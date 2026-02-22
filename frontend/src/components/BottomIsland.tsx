import { useEffect, useRef, useState, type ReactNode } from 'react'

type PopupRenderContext = {
  close: () => void
}

type BottomIslandAction = {
  id: string
  label: ReactNode
  testId?: string
  disabled?: boolean
  className?: string
  onClick?: () => void
  renderPopup?: (context: PopupRenderContext) => ReactNode
}

type BottomIslandProps = {
  actions: BottomIslandAction[]
  className?: string
}

export function BottomIsland({ actions, className }: BottomIslandProps): ReactNode {
  const [openActionId, setOpenActionId] = useState<string | null>(null)
  const rootRef = useRef<HTMLDivElement | null>(null)

  useEffect(() => {
    const onDocumentMouseDown = (event: MouseEvent) => {
      const root = rootRef.current
      if (!root) return
      if (event.target instanceof Node && root.contains(event.target)) return
      setOpenActionId(null)
    }

    const onDocumentKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        setOpenActionId(null)
      }
    }

    document.addEventListener('mousedown', onDocumentMouseDown)
    document.addEventListener('keydown', onDocumentKeyDown)
    return () => {
      document.removeEventListener('mousedown', onDocumentMouseDown)
      document.removeEventListener('keydown', onDocumentKeyDown)
    }
  }, [])

  return (
    <div className={`bottom-island ${className ?? ''}`.trim()} ref={rootRef}>
      {actions.map((action) => {
        const hasPopup = action.renderPopup !== undefined
        const isOpen = openActionId === action.id

        return (
          <div className="bottom-island-item" key={action.id}>
            <button
              type="button"
              className={`bottom-island-action ${action.className ?? ''} ${isOpen ? 'is-open' : ''}`.trim()}
              data-testid={action.testId}
              disabled={action.disabled}
              aria-expanded={hasPopup ? isOpen : undefined}
              aria-haspopup={hasPopup ? 'dialog' : undefined}
              onClick={() => {
                if (hasPopup) {
                  setOpenActionId((current) => (current === action.id ? null : action.id))
                  return
                }
                setOpenActionId(null)
                action.onClick?.()
              }}
            >
              {action.label}
            </button>
            {hasPopup && isOpen && (
              <div className="bottom-island-popup" role="dialog">
                {action.renderPopup?.({
                  close: () => setOpenActionId(null),
                })}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}
