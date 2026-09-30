<?php

declare(strict_types=1);

namespace Shop\Ordering;

/**
 * The version is not business data. It is the boundary between what this
 * process read and what the database holds now, and it is the only field in
 * this object that the storage layer is allowed to move behind our back.
 */
final class Order
{
    public function __construct(
        public readonly int $id,
        public readonly string $status,
        public readonly int $totalMinor,
        public readonly int $version,
    ) {
    }

    /**
     * Deriving new state does NOT bump the version here. The version is the
     * predicate of the UPDATE, not a counter this process owns: two writers
     * holding v1 must both present v1, so that the loser's UPDATE matches
     * zero rows. If we incremented locally, every writer would invent its own
     * version and nothing would ever conflict.
     */
    public function withStatus(string $status): self
    {
        return new self($this->id, $status, $this->totalMinor, $this->version);
    }
}
