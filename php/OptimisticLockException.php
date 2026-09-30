<?php

declare(strict_types=1);

namespace Shop\Ordering;

/**
 * Raised when the row moved between the read and the write. This is a
 * concurrency signal, not a storage failure: the caller is expected to
 * reload, re-apply its intent and try again, or to surface the conflict.
 */
final class OptimisticLockException extends \RuntimeException
{
    public function __construct(
        public readonly int $id,
        public readonly int $expectedVersion,
        public readonly int $actualVersion,
    ) {
        parent::__construct(sprintf(
            'Order %d: expected version %d, found %d',
            $id,
            $expectedVersion,
            $actualVersion,
        ));
    }
}
